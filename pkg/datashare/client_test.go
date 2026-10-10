package datashare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// init removes the pause between attempts so the retry tests run instantly.
func init() { retryDelay = 0 }

// collection renders a FeatureCollection of point features with the given ids.
func collection(ids ...string) string {
	feats := make([]string, len(ids))
	for i, id := range ids {
		feats[i] = fmt.Sprintf(`{"type":"Feature","properties":{"fulcrum_id":%q,"n":%d},"geometry":{"type":"Point","coordinates":[%d,1]}}`, id, i, i)
	}
	return `{"type":"FeatureCollection","features":[` + strings.Join(feats, ",") + `]}`
}

// TestDownloadFollowsPages checks that every page is fetched and merged, that
// paging stops after the first short page, and that the sink receives one
// valid FeatureCollection.
func TestDownloadFollowsPages(t *testing.T) {
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Query().Get("page"))
		switch r.URL.Query().Get("page") {
		case "1":
			fmt.Fprint(w, collection("a", "b", "c"))
		case "2":
			fmt.Fprint(w, collection("d", "e", "f"))
		case "3":
			fmt.Fprint(w, collection("g"))
		default:
			fmt.Fprint(w, collection())
		}
	}))
	defer srv.Close()

	var sink bytes.Buffer
	c := &Client{URL: srv.URL + "/shares/abc.geojson"}
	res, err := c.Download(context.Background(), &sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Features) != 7 || res.Pages != 3 {
		t.Errorf("got %d features in %d pages, want 7 in 3", len(res.Features), res.Pages)
	}
	if got := strings.Join(requests, ","); got != "1,2,3" {
		t.Errorf("requested pages %s, want 1,2,3 (stop after the short page)", got)
	}

	var merged struct {
		Type     string            `json:"type"`
		Features []json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(sink.Bytes(), &merged); err != nil {
		t.Fatalf("merged GeoJSON is invalid: %v", err)
	}
	if merged.Type != "FeatureCollection" || len(merged.Features) != 7 {
		t.Errorf("merged GeoJSON has type %q and %d features", merged.Type, len(merged.Features))
	}
}

// TestDownloadStopsAfterEmptyPage checks that a share fitting in one page
// ends at the empty second page, which is not counted.
func TestDownloadStopsAfterEmptyPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, collection("a", "b"))
			return
		}
		fmt.Fprint(w, collection())
	}))
	defer srv.Close()

	res, err := (&Client{URL: srv.URL}).Download(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Features) != 2 || res.Pages != 1 {
		t.Errorf("got %d features in %d pages, want 2 in 1", len(res.Features), res.Pages)
	}
}

// TestDownloadStopsWhenServerIgnoresPaging checks that a server returning the
// same records for every page does not cause an endless loop or duplicates.
func TestDownloadStopsWhenServerIgnoresPaging(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, collection("a", "b"))
	}))
	defer srv.Close()

	res, err := (&Client{URL: srv.URL}).Download(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Features) != 2 || res.Duplicates != 2 || calls != 2 {
		t.Errorf("got %d features, %d duplicates, %d calls; want 2, 2, 2", len(res.Features), res.Duplicates, calls)
	}
}

// TestDownloadNotFound checks that HTTP 404 on the first page is reported as
// a missing share.
func TestDownloadNotFound(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	_, err := (&Client{URL: srv.URL}).Download(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "share not found") {
		t.Errorf("got error %v, want share not found", err)
	}
}

// TestDownloadSendsCredentialsOnlyWhenRefused checks the Basic auth fallback:
// the first request is anonymous, credentials follow a 401, and missing or
// wrong credentials produce a clear error.
func TestDownloadSendsCredentialsOnlyWhenRefused(t *testing.T) {
	var sawAuth []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		sawAuth = append(sawAuth, ok)
		if !ok || user != "me@example.com" || pass != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, collection("a"))
			return
		}
		fmt.Fprint(w, collection())
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL, Username: "me@example.com", Password: "secret"}
	res, err := c.Download(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Features) != 1 {
		t.Errorf("got %d features, want 1", len(res.Features))
	}
	if len(sawAuth) < 2 || sawAuth[0] || !sawAuth[1] {
		t.Errorf("auth per request = %v, want anonymous first and credentials after the 401", sawAuth)
	}

	_, err = (&Client{URL: srv.URL}).Download(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "FULCRUM_USERNAME") {
		t.Errorf("without credentials got error %v, want a hint to set them", err)
	}

	_, err = (&Client{URL: srv.URL, Username: "me@example.com", Password: "wrong"}).Download(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("with wrong credentials got error %v, want rejected", err)
	}
}

// TestDownloadRetriesServerError checks that a page failing with a 5xx
// response is requested again.
func TestDownloadRetriesServerError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, collection("a"))
			return
		}
		fmt.Fprint(w, collection())
	}))
	defer srv.Close()

	res, err := (&Client{URL: srv.URL}).Download(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Features) != 1 {
		t.Errorf("got %d features, want 1", len(res.Features))
	}
}

// TestDownloadRejectsHTML checks that an HTML response, such as a sign-in
// page, is reported instead of being parsed as data.
func TestDownloadRejectsHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html>Sign in</html>")
	}))
	defer srv.Close()

	_, err := (&Client{URL: srv.URL}).Download(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "expected GeoJSON") {
		t.Errorf("got error %v, want expected GeoJSON", err)
	}
}
