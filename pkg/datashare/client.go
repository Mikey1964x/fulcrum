// Package datashare downloads the GeoJSON of a Fulcrum data share.
package datashare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mikey1964x/fulcrum/pkg/geojson"
)

// idProperty is the record identifier Fulcrum puts on every feature.
const idProperty = "fulcrum_id"

var (
	errNotFound = errors.New("share not found")

	// retryDelay is the pause before a failed page is requested again.
	retryDelay = 2 * time.Second
)

// Client downloads one data share. A share is served in pages, so a single
// request for the share URL only returns the first part of a large share.
type Client struct {
	URL string

	// Username and Password are optional. They are sent as HTTP Basic auth
	// only after the server has refused an anonymous request.
	Username string
	Password string

	HTTP     *http.Client
	Progress io.Writer

	useAuth bool
}

// Result is the outcome of a download.
type Result struct {
	Features   []*geojson.Feature
	Pages      int
	Duplicates int
}

type item struct {
	raw     json.RawMessage
	feature *geojson.Feature
}

// retryable marks an error worth one more attempt.
type retryable struct{ error }

func (r retryable) Unwrap() error { return r.error }

// Download fetches every page of the share. If sink is not nil, the features
// are also written to it as one merged FeatureCollection, byte for byte as
// the server sent them.
func (c *Client) Download(ctx context.Context, sink io.Writer) (*Result, error) {
	res := &Result{}
	seen := make(map[string]struct{})
	largest := 0

	if err := writeSink(sink, `{"type":"FeatureCollection","features":[`); err != nil {
		return nil, err
	}
	for page := 1; ; page++ {
		items, err := c.fetchPageWithRetry(ctx, page)
		if page > 1 && errors.Is(err, errNotFound) {
			break
		}
		if errors.Is(err, errNotFound) {
			return nil, errors.New("share not found (HTTP 404): check the share URL and that sharing is still enabled for the app")
		}
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
		if len(items) == 0 {
			break
		}
		res.Pages++

		added := 0
		for _, it := range items {
			if id, ok := it.feature.Properties.Get(idProperty); ok {
				if s, _ := id.(string); s != "" {
					if _, dup := seen[s]; dup {
						res.Duplicates++
						continue
					}
					seen[s] = struct{}{}
				}
			}
			sep := ",\n"
			if len(res.Features) == 0 {
				sep = "\n"
			}
			if err := writeSink(sink, sep, string(it.raw)); err != nil {
				return nil, err
			}
			res.Features = append(res.Features, it.feature)
			added++
		}
		c.progressf("page %d: %d features (%d total)\n", page, len(items), len(res.Features))

		// A page that is shorter than an earlier one is the last page; a page
		// with nothing new means the server is not paging at all.
		if added == 0 || len(items) < largest {
			break
		}
		largest = len(items)
	}
	if err := writeSink(sink, "\n]}\n"); err != nil {
		return nil, err
	}
	return res, nil
}

func (c *Client) fetchPageWithRetry(ctx context.Context, page int) ([]item, error) {
	items, err := c.fetchPage(ctx, page)
	var r retryable
	if err == nil || !errors.As(err, &r) || ctx.Err() != nil {
		return items, err
	}
	c.progressf("page %d failed (%v), retrying\n", page, err)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(retryDelay):
	}
	return c.fetchPage(ctx, page)
}

func (c *Client) fetchPage(ctx context.Context, page int) ([]item, error) {
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("page", strconv.Itoa(page))
	u.RawQuery = q.Encode()

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/geo+json, application/json")
		if c.useAuth {
			req.SetBasicAuth(c.Username, c.Password)
		}
		resp, err := c.httpClient().Do(req)
		if err != nil {
			return nil, retryable{err}
		}

		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			resp.Body.Close()
			if c.useAuth {
				return nil, fmt.Errorf("the server rejected FULCRUM_USERNAME/FULCRUM_PASSWORD (HTTP %d)", resp.StatusCode)
			}
			if c.Username == "" || c.Password == "" {
				return nil, fmt.Errorf("the share requires a login (HTTP %d): set FULCRUM_USERNAME and FULCRUM_PASSWORD", resp.StatusCode)
			}
			c.useAuth = true
			continue
		case resp.StatusCode == http.StatusNotFound:
			resp.Body.Close()
			return nil, errNotFound
		case resp.StatusCode >= 500:
			resp.Body.Close()
			return nil, retryable{fmt.Errorf("server error: HTTP %d", resp.StatusCode)}
		case resp.StatusCode != http.StatusOK:
			resp.Body.Close()
			return nil, fmt.Errorf("unexpected response: HTTP %d", resp.StatusCode)
		}

		items, err := decodePage(resp)
		resp.Body.Close()
		return items, err
	}
}

func decodePage(resp *http.Response) ([]item, error) {
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "html") {
		return nil, fmt.Errorf("expected GeoJSON but the server sent %s (a login or error page?)", ct)
	}
	var items []item
	err := geojson.DecodeFeatures(resp.Body, func(raw json.RawMessage, f *geojson.Feature) error {
		items = append(items, item{raw: raw, feature: f})
		return nil
	})
	if err != nil {
		// Most often a connection dropped part-way through a large page.
		return nil, retryable{fmt.Errorf("reading GeoJSON: %w", err)}
	}
	return items, nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) progressf(format string, args ...any) {
	if c.Progress != nil {
		fmt.Fprintf(c.Progress, format, args...)
	}
}

func writeSink(sink io.Writer, parts ...string) error {
	if sink == nil {
		return nil
	}
	for _, p := range parts {
		if _, err := io.WriteString(sink, p); err != nil {
			return fmt.Errorf("writing GeoJSON: %w", err)
		}
	}
	return nil
}
