package photos

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mikey1964x/fulcrum/pkg/geojson"
)

func init() { retryDelay = 0 }

const (
	idA = "2557da7f-a25d-4d57-95b3-9db28013b8b6"
	idB = "8a52e7e2-e7c4-4bcd-8ab9-e82cb3cbabfc"
	idC = "0f7815f4-60c3-48ca-b0d5-cee0ae96ad30"
)

// features decodes one feature per JSON properties object.
func features(t *testing.T, props ...string) []*geojson.Feature {
	t.Helper()
	var out []*geojson.Feature
	for _, p := range props {
		f := new(geojson.Feature)
		if err := json.Unmarshal([]byte(`{"type":"Feature","properties":`+p+`,"geometry":null}`), f); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func TestCollect(t *testing.T) {
	const share = "https://web.fulcrumapp.com/shares/abc123.geojson"
	const view = "https://web.fulcrumapp.com/shares/abc123/photos/view?photos="
	fs := features(t,
		`{"xfmr_photo":"`+idA+`","xfmr_photo_url":"`+view+idA+`"}`,
		`{"xfmr_photo_url":"`+view+idB+`%2C`+strings.ToUpper(idA)+`","enclosure_photo_url":"`+view+idC+`"}`,
		`{"xfmr_photo_url":null,"enclosure_photo_url":"","count_url":7}`,
		`{"a_url":"https://evil.example/shares/abc123/photos/view?photos=`+idC+`"}`,
		`{"b_url":"https://web.fulcrumapp.com/shares/abc123/videos/view?videos=`+idC+`"}`,
		`{"c_url":"http://web.fulcrumapp.com/shares/abc123/photos/view?photos=`+idC+`"}`,
		`{"d_url":"`+view+`not-a-guid"}`,
		`{"website":"`+view+idC+`x"}`,
	)

	refs, skipped := Collect(fs, share)

	want := []Ref{
		{idA, "https://web.fulcrumapp.com/shares/abc123/photos/" + idA},
		{idB, "https://web.fulcrumapp.com/shares/abc123/photos/" + idB},
		{idC, "https://web.fulcrumapp.com/shares/abc123/photos/" + idC},
	}
	if len(refs) != len(want) {
		t.Fatalf("got %d refs %v, want %d", len(refs), refs, len(want))
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Errorf("ref %d = %v, want %v", i, refs[i], want[i])
		}
	}
	if skipped != 4 {
		t.Errorf("skipped = %d, want 4", skipped)
	}
}

// refsFor returns a Ref for each id, pointing at srv.
func refsFor(srv *httptest.Server, ids ...string) []Ref {
	var refs []Ref
	for _, id := range ids {
		refs = append(refs, Ref{ID: id, URL: srv.URL + "/shares/tok/photos/" + id})
	}
	return refs
}

// names returns the file names in dir.
func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestDownloadSavesPhotos(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch id := filepath.Base(r.URL.Path); id {
		case idA:
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("jpeg " + id))
		case idB:
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("png " + id))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := filepath.Join(t.TempDir(), "photos")
	d := &Downloader{Dir: dir, Workers: 2}
	res, err := d.Download(context.Background(), refsFor(srv, idA, idB, idC))
	if err != nil {
		t.Fatal(err)
	}
	if res.Downloaded != 2 || res.Missing != 1 || res.Failed != 0 || res.Existing != 0 {
		t.Errorf("result = %+v", res)
	}
	got, err := os.ReadFile(filepath.Join(dir, idA+".jpg"))
	if err != nil || string(got) != "jpeg "+idA {
		t.Errorf("%s.jpg = %q, %v", idA, got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, idB+".png")); err != nil {
		t.Error(err)
	}
	if n := names(t, dir); len(n) != 2 {
		t.Errorf("files = %v, want 2", n)
	}
}

func TestDownloadSkipsExisting(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("new"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, idA+".jpg"), []byte("old"), 0o644)
	// An unfinished download does not count as present.
	os.WriteFile(filepath.Join(dir, idB+partSuffix), []byte("half"), 0o644)

	d := &Downloader{Dir: dir}
	res, err := d.Download(context.Background(), refsFor(srv, idA, idB))
	if err != nil {
		t.Fatal(err)
	}
	if res.Existing != 1 || res.Downloaded != 1 || requests.Load() != 1 {
		t.Errorf("result = %+v, requests = %d", res, requests.Load())
	}
	if got, _ := os.ReadFile(filepath.Join(dir, idA+".jpg")); string(got) != "old" {
		t.Errorf("existing photo was overwritten: %q", got)
	}
	for _, n := range names(t, dir) {
		if strings.HasSuffix(n, partSuffix) {
			t.Errorf("left behind %s", n)
		}
	}
}

func TestDownloadRetriesServerError(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	d := &Downloader{Dir: t.TempDir()}
	res, err := d.Download(context.Background(), refsFor(srv, idA))
	if err != nil {
		t.Fatal(err)
	}
	if res.Downloaded != 1 || res.Failed != 0 || requests.Load() != 2 {
		t.Errorf("result = %+v, requests = %d", res, requests.Load())
	}
}

func TestDownloadCountsFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if filepath.Base(r.URL.Path) == idA {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		// A login page served with status 200.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html>sign in</html>"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	d := &Downloader{Dir: dir}
	res, err := d.Download(context.Background(), refsFor(srv, idA, idB))
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 2 || res.Downloaded != 0 || len(res.Errors) != 2 {
		t.Errorf("result = %+v", res)
	}
	if n := names(t, dir); len(n) != 0 {
		t.Errorf("files = %v, want none", n)
	}
}

func TestDownloadSendsCredentialsOnlyWhenRefused(t *testing.T) {
	var mu sync.Mutex
	anonymous := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			mu.Lock()
			anonymous++
			mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if user != "me" || pass != "secret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	d := &Downloader{Dir: t.TempDir(), Username: "me", Password: "secret", Workers: 1}
	res, err := d.Download(context.Background(), refsFor(srv, idA, idB, idC))
	if err != nil {
		t.Fatal(err)
	}
	if res.Downloaded != 3 {
		t.Errorf("result = %+v", res)
	}
	if anonymous != 1 {
		t.Errorf("anonymous requests = %d, want 1", anonymous)
	}

	// Without credentials the run stops instead of failing every photo.
	d = &Downloader{Dir: t.TempDir()}
	if _, err := d.Download(context.Background(), refsFor(srv, idA, idB)); err == nil || !strings.Contains(err.Error(), "require a login") {
		t.Errorf("err = %v, want a login error", err)
	}

	// Refused credentials stop the run as well.
	d = &Downloader{Dir: t.TempDir(), Username: "me", Password: "wrong"}
	if _, err := d.Download(context.Background(), refsFor(srv, idA)); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("err = %v, want a rejection error", err)
	}
}

func TestDownloadStopsWhenCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	d := &Downloader{Dir: dir}
	if _, err := d.Download(ctx, refsFor(srv, idA, idB)); err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if n := names(t, dir); len(n) != 0 {
		t.Errorf("files = %v, want none", n)
	}
}
