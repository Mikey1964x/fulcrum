// Package photos downloads the pictures attached to the records of a Fulcrum
// data share.
//
// A share's photo fields come with a companion attribute whose name ends in
// "_url" and whose value links to a web page showing the photos:
//
//	https://web.fulcrumapp.com/shares/<token>/photos/view?photos=<id>,<id>
//
// The page only embeds reduced copies. The picture itself is served at
//
//	https://web.fulcrumapp.com/shares/<token>/photos/<id>
//
// under the same access control as the share: the token in the link. [Collect]
// turns the attribute values into those direct links and a [Downloader] saves
// each picture as <id>.jpg. If the server does refuse an anonymous request,
// the Downloader retries with HTTP Basic auth when it has been given
// credentials.
package photos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mikey1964x/fulcrum/pkg/geojson"
)

// urlSuffix marks the attributes that link to a record's photos.
const urlSuffix = "_url"

// partSuffix is the extension of a picture that is still being written.
const partSuffix = ".part"

// defaultWorkers is the number of parallel downloads when Workers is not set.
const defaultWorkers = 8

// progressEvery is the number of photos between two progress lines.
const progressEvery = 500

// maxErrors is the number of failures kept in Result.Errors.
const maxErrors = 10

// maxRetryAfter caps the wait a server can ask for with Retry-After.
const maxRetryAfter = time.Minute

// retryDelay is the pause before a failed photo is requested again. It is a
// variable so tests can remove the wait.
var retryDelay = 2 * time.Second

// idPattern matches a photo identifier, which is a GUID.
var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)

// errNotFound reports that the server answered HTTP 404 for a photo.
var errNotFound = errors.New("photo not found")

// extensions maps the media type of a picture to its file extension.
var extensions = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
	"image/heic": ".heic",
}

// Ref identifies one photo to download.
type Ref struct {
	// ID is the photo's GUID, used as the base name of the saved file.
	ID string

	// URL is the direct link to the picture.
	URL string
}

// Collect returns the photos referenced by the "_url" attributes of features,
// each once, in the order first seen. shareURL is the link of the data share.
//
// Only links to a photo page on the share's own host are accepted, so the
// contents of the data can never send requests, or the login, to another
// site. skipped is the number of non-empty "_url" values that were not such a
// link, plus the identifiers in a link that are not GUIDs.
func Collect(features []*geojson.Feature, shareURL string) (refs []Ref, skipped int) {
	share, err := url.Parse(shareURL)
	if err != nil {
		return nil, 0
	}
	seen := make(map[string]struct{})
	for _, f := range features {
		for _, prop := range f.Properties {
			if !strings.HasSuffix(prop.Key, urlSuffix) {
				continue
			}
			value, _ := prop.Value.(string)
			if value == "" {
				continue
			}
			base, ids, ok := parseViewLink(value, share)
			if !ok {
				skipped++
				continue
			}
			for _, id := range ids {
				if !idPattern.MatchString(id) {
					skipped++
					continue
				}
				id = strings.ToLower(id)
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
				refs = append(refs, Ref{ID: id, URL: base + id})
			}
		}
	}
	return refs, skipped
}

// parseViewLink splits the link of a photo page into the prefix of the direct
// photo links (ending in "/photos/") and the photo identifiers. ok is false
// if link is not a photo page on the host of share.
func parseViewLink(link string, share *url.URL) (base string, ids []string, ok bool) {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != share.Scheme || !strings.EqualFold(u.Host, share.Host) {
		return "", nil, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "shares" || parts[1] == "" || parts[2] != "photos" || parts[3] != "view" {
		return "", nil, false
	}
	list := u.Query().Get("photos")
	if list == "" {
		return "", nil, false
	}
	base = u.Scheme + "://" + u.Host + "/shares/" + url.PathEscape(parts[1]) + "/photos/"
	return base, strings.Split(list, ","), true
}

// Downloader saves photos to a directory, several at a time.
type Downloader struct {
	// Dir is the directory the pictures are written to. It is created if it
	// does not exist.
	Dir string

	// Username and Password are optional. They are sent as HTTP Basic auth
	// only after the server has refused an anonymous request.
	Username string
	Password string

	// HTTP is the client used for requests. If nil, http.DefaultClient is
	// used.
	HTTP *http.Client

	// Workers is the number of parallel downloads. If it is less than 1,
	// 8 are used.
	Workers int

	// Progress, if not nil, receives a line at the start and one for every
	// 500 photos handled.
	Progress io.Writer

	// useAuth records that the server has asked for credentials, so later
	// requests send them straight away.
	useAuth atomic.Bool
}

// Result is the outcome of a download.
type Result struct {
	// Downloaded is the number of pictures saved by this run.
	Downloaded int

	// Existing is the number of photos skipped because their file was
	// already in the directory.
	Existing int

	// Missing is the number of photos the server no longer has (HTTP 404).
	Missing int

	// Failed is the number of photos that could not be downloaded.
	Failed int

	// Errors holds the first few failures.
	Errors []error
}

// retryable marks an error worth one more attempt, after waiting for wait.
type retryable struct {
	error
	wait time.Duration
}

// Unwrap returns the wrapped error, for errors.Is and errors.As.
func (r retryable) Unwrap() error { return r.error }

// fatal marks an error that every other photo would run into as well.
type fatal struct{ error }

// Unwrap returns the wrapped error, for errors.Is and errors.As.
func (f fatal) Unwrap() error { return f.error }

// Download saves every photo of refs that is not already in Dir, as
// <id>.jpg (or the extension matching the picture's type). A picture is
// written under a temporary name and renamed once complete, so an interrupted
// run leaves no truncated pictures and can simply be started again.
//
// A photo that fails is retried once and then counted in the Result; it does
// not stop the others. Download only returns an error if Dir cannot be used,
// if the server demands a login that is missing or refused, or if ctx is
// done. The Result is valid even then.
func (d *Downloader) Download(ctx context.Context, refs []Ref) (*Result, error) {
	res := &Result{}
	if err := os.MkdirAll(d.Dir, 0o755); err != nil {
		return res, err
	}
	have, err := d.existing()
	if err != nil {
		return res, err
	}
	var todo []Ref
	for _, ref := range refs {
		if _, ok := have[ref.ID]; ok {
			res.Existing++
			continue
		}
		todo = append(todo, ref)
	}
	d.progressf("%d photos to download (%d already present)\n", len(todo), res.Existing)
	if len(todo) == 0 {
		return res, nil
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	workers := d.Workers
	if workers < 1 {
		workers = defaultWorkers
	}
	jobs := make(chan Ref)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		done int
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ref := range jobs {
				err := d.fetchWithRetry(ctx, ref)
				if ctx.Err() != nil {
					continue
				}
				var f fatal
				if errors.As(err, &f) {
					cancel(f.error)
					continue
				}

				mu.Lock()
				switch {
				case err == nil:
					res.Downloaded++
				case errors.Is(err, errNotFound):
					res.Missing++
				default:
					res.Failed++
					if len(res.Errors) < maxErrors {
						res.Errors = append(res.Errors, fmt.Errorf("photo %s: %w", ref.ID, err))
					}
				}
				done++
				if done%progressEvery == 0 {
					d.progressf("photos: %d of %d\n", done, len(todo))
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, ref := range todo {
		select {
		case jobs <- ref:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	if ctx.Err() != nil {
		return res, context.Cause(ctx)
	}
	return res, nil
}

// existing returns the identifiers of the photos already saved in Dir.
// Unfinished downloads are not counted.
func (d *Downloader) existing() (map[string]struct{}, error) {
	entries, err := os.ReadDir(d.Dir)
	if err != nil {
		return nil, err
	}
	have := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		name := e.Name()
		ext := filepath.Ext(name)
		if e.IsDir() || ext == partSuffix {
			continue
		}
		have[strings.ToLower(strings.TrimSuffix(name, ext))] = struct{}{}
	}
	return have, nil
}

// fetchWithRetry downloads one photo and, if that fails with a retryable
// error, waits and tries once more. It does not retry once ctx is done.
func (d *Downloader) fetchWithRetry(ctx context.Context, ref Ref) error {
	err := d.fetch(ctx, ref)
	var r retryable
	if err == nil || !errors.As(err, &r) || ctx.Err() != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(r.wait):
	}
	return d.fetch(ctx, ref)
}

// fetch requests one photo and saves it.
//
// On HTTP 401 or 403 it switches the Downloader to Basic auth and repeats the
// request; it returns a fatal error if no credentials are set or they are
// refused. It returns errNotFound for HTTP 404, and a retryable error for
// network failures, HTTP 429, 5xx responses and bodies that end early.
func (d *Downloader) fetch(ctx context.Context, ref Ref) error {
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.URL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "image/*")
		sentAuth := d.useAuth.Load()
		if sentAuth {
			req.SetBasicAuth(d.Username, d.Password)
		}
		resp, err := d.httpClient().Do(req)
		if err != nil {
			return retryable{err, retryDelay}
		}

		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			resp.Body.Close()
			if sentAuth {
				return fatal{fmt.Errorf("the server rejected FULCRUM_USERNAME/FULCRUM_PASSWORD for the photos (HTTP %d)", resp.StatusCode)}
			}
			if d.Username == "" || d.Password == "" {
				return fatal{fmt.Errorf("the photos require a login (HTTP %d): set FULCRUM_USERNAME and FULCRUM_PASSWORD", resp.StatusCode)}
			}
			d.useAuth.Store(true)
			continue
		case resp.StatusCode == http.StatusNotFound:
			resp.Body.Close()
			return errNotFound
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			wait := retryAfter(resp)
			resp.Body.Close()
			return retryable{fmt.Errorf("server error: HTTP %d", resp.StatusCode), wait}
		case resp.StatusCode != http.StatusOK:
			resp.Body.Close()
			return fmt.Errorf("unexpected response: HTTP %d", resp.StatusCode)
		}

		err = d.save(ref, resp)
		resp.Body.Close()
		return err
	}
}

// save writes the body of a successful response to Dir. It does not close
// the body. An HTML response is rejected, since it is a login or error page
// rather than a picture.
func (d *Downloader) save(ref Ref, resp *http.Response) (err error) {
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if strings.Contains(mediaType, "html") {
		return fmt.Errorf("expected a picture but the server sent %s (a login or error page?)", mediaType)
	}
	ext, ok := extensions[mediaType]
	if !ok {
		ext = ".jpg"
	}

	part := filepath.Join(d.Dir, ref.ID+partSuffix)
	file, err := os.Create(part)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(part)
		}
	}()
	n, err := io.Copy(file, resp.Body)
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		// Most often a connection dropped part-way through the picture.
		return retryable{fmt.Errorf("reading picture: %w", err), retryDelay}
	}
	if n == 0 {
		return retryable{errors.New("the server sent an empty picture"), retryDelay}
	}
	return os.Rename(part, filepath.Join(d.Dir, ref.ID+ext))
}

// retryAfter returns how long to wait before repeating the request that got
// resp: the number of seconds in its Retry-After header, capped at
// maxRetryAfter, or retryDelay if the header is absent or not a number.
func retryAfter(resp *http.Response) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After")))
	if err != nil || secs < 0 {
		return retryDelay
	}
	return min(time.Duration(secs)*time.Second, maxRetryAfter)
}

// httpClient returns d.HTTP, or http.DefaultClient if it is nil.
func (d *Downloader) httpClient() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return http.DefaultClient
}

// progressf writes a formatted message to d.Progress, if one is set.
func (d *Downloader) progressf(format string, args ...any) {
	if d.Progress != nil {
		fmt.Fprintf(d.Progress, format, args...)
	}
}
