package dashboard

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mikey1964x/fulcrum/pkg/geojson"
)

const (
	photoA = "2557da7f-a25d-4d57-95b3-9db28013b8b6"
	photoB = "8a52e7e2-e7c4-4bcd-8ab9-e82cb3cbabfc"
	share  = "https://web.fulcrumapp.com/shares/tok.geojson"
	view   = "https://web.fulcrumapp.com/shares/tok/photos/view?photos="
)

// records are the properties of the test share's features.
var records = []string{
	`{"status":"Inspection Complete","inspection_type":"Transformer","address_sub_admin_area":"Horry County","inspection_date":"2024-05-28","exterior":"Good","lock":"Lock in Place,\"Lock Missing, Replaced\"","xfmr_photo_url":"` + view + photoA + `%2C` + photoB + `"}`,
	`{"status":"Inspection Complete","inspection_type":"Transformer","address_sub_admin_area":"Horry County","inspection_date":"2024-07-02","exterior":"Slight Rust","lock":"Lock in Place"}`,
	`{"status":"Not Inspected","inspection_type":"Transformer","address_sub_admin_area":"Georgetown County","inspection_date":"2024-05-01","exterior":"Good","lock":null}`,
	`{"status":"No Visual Access, Structure ","inspection_type":"Switch Gear","address_sub_admin_area":null,"inspection_date":null,"exterior":null}`,
}

// testFeatures decodes records into features.
func testFeatures(t *testing.T) []*geojson.Feature {
	t.Helper()
	var out []*geojson.Feature
	for _, p := range records {
		f := new(geojson.Feature)
		if err := json.Unmarshal([]byte(`{"type":"Feature","properties":`+p+`,"geometry":null}`), f); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func TestCompute(t *testing.T) {
	st := Compute(testFeatures(t), Filter{})

	if st.Total != 4 || st.Inspected != 2 || st.NotInspected != 1 || st.Other != 1 {
		t.Errorf("totals = %d/%d/%d/%d", st.Total, st.Inspected, st.NotInspected, st.Other)
	}
	wantStatuses := []Count{{"Inspection Complete", 2}, {"No Visual Access, Structure", 1}, {"Not Inspected", 1}}
	if !reflect.DeepEqual(st.Statuses, wantStatuses) {
		t.Errorf("statuses = %v", st.Statuses)
	}
	wantTypes := []Split{
		{Label: "Transformer", Total: 3, Inspected: 2, NotInspected: 1},
		{Label: "Switch Gear", Total: 1, Other: 1},
	}
	if !reflect.DeepEqual(st.Types, wantTypes) {
		t.Errorf("types = %v", st.Types)
	}
	if len(st.Counties) != 3 || st.Counties[0].Label != "Horry County" || st.Counties[2].Label != unknown {
		t.Errorf("counties = %v", st.Counties)
	}

	// Only the findings with a value are reported; a record with several
	// choices counts under each.
	if len(st.Conditions) != 2 {
		t.Fatalf("conditions = %v", st.Conditions)
	}
	ext, lock := st.Conditions[0], st.Conditions[1]
	if ext.Key != "exterior" || ext.Answered != 3 || ext.Blank != 1 ||
		!reflect.DeepEqual(ext.Counts, []Count{{"Good", 2}, {"Slight Rust", 1}}) {
		t.Errorf("exterior = %+v", ext)
	}
	if lock.Key != "lock" || lock.Answered != 2 || lock.Blank != 2 ||
		!reflect.DeepEqual(lock.Counts, []Count{{"Lock in Place", 2}, {"Lock Missing, Replaced", 1}}) {
		t.Errorf("lock = %+v", lock)
	}

	// Months without records are filled in.
	wantMonths := []Count{{"2024-05", 2}, {"2024-06", 0}, {"2024-07", 1}}
	if !reflect.DeepEqual(st.Months, wantMonths) {
		t.Errorf("months = %v", st.Months)
	}
}

func TestChoices(t *testing.T) {
	for value, want := range map[string][]string{
		`Good`:                                   {"Good"},
		`Lock in Place,Lock Not in View`:         {"Lock in Place", "Lock Not in View"},
		`"Lock Missing, Replaced",Lock in Place`: {"Lock Missing, Replaced", "Lock in Place"},
		`Old Label, New Label added`:             {"Old Label, New Label added"},
		` OK ,`:                                  {"OK ,"},
	} {
		if got := choices(value); !reflect.DeepEqual(got, want) {
			t.Errorf("choices(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestComputeFilter(t *testing.T) {
	fs := testFeatures(t)
	if st := Compute(fs, Filter{Type: "Transformer", County: "Horry County"}); st.Total != 2 || st.Inspected != 2 {
		t.Errorf("type+county: %+v", st)
	}
	if st := Compute(fs, Filter{County: unknown}); st.Total != 1 || st.Other != 1 {
		t.Errorf("unknown county: %+v", st)
	}
	st := Compute(fs, Filter{Status: "No such status"})
	if st.Total != 0 || len(st.Months) != 0 || len(st.Conditions) != 0 {
		t.Errorf("no match: %+v", st)
	}
	// Empty lists must encode as [], not null, for the page.
	if b, _ := json.Marshal(st); strings.Contains(string(b), "null") {
		t.Errorf("empty stats encode with null: %s", b)
	}
}

func TestListOptions(t *testing.T) {
	got := ListOptions(testFeatures(t))
	want := Options{
		Types:    []string{"Switch Gear", "Transformer"},
		Counties: []string{"Georgetown County", "Horry County", unknown},
		Statuses: []string{"Inspection Complete", "No Visual Access, Structure", "Not Inspected"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("options = %+v", got)
	}
}

// newServer writes the test share to a temporary directory, with photos if
// asked, and returns a test server for its dashboard.
func newServer(t *testing.T, withPhotos bool) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	var doc bytes.Buffer
	doc.WriteString(`{"type":"FeatureCollection","features":[`)
	for i, p := range records {
		if i > 0 {
			doc.WriteString(",")
		}
		doc.WriteString(`{"type":"Feature","properties":` + p + `,"geometry":null}`)
	}
	doc.WriteString("]}")
	path := filepath.Join(dir, "tok.geojson")
	if err := os.WriteFile(path, doc.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	photosDir := filepath.Join(dir, "photos")
	if withPhotos {
		os.Mkdir(photosDir, 0o755)
		os.WriteFile(filepath.Join(photosDir, photoA+".jpg"), []byte("picture a"), 0o644)
		os.WriteFile(filepath.Join(photosDir, photoB+".part"), []byte("half"), 0o644)
	}
	s := &Server{Name: "tok", GeoJSONPath: path, PhotosDir: photosDir, ShareURL: share}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, path
}

// get requests path from srv and returns the response and its body.
func get(t *testing.T, srv *httptest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func TestServerPageAndAPI(t *testing.T) {
	srv, path := newServer(t, true)

	resp, body := get(t, srv, "/")
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Inspection dashboard") {
		t.Errorf("page: %d", resp.StatusCode)
	}
	if resp, _ := get(t, srv, "/nothing"); resp.StatusCode != 404 {
		t.Errorf("unknown path: %d", resp.StatusCode)
	}

	var info Info
	_, body = get(t, srv, "/api/info")
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	if info.Name != "tok" || info.Features != 4 || info.PhotosReferenced != 2 || info.PhotosOnDisk != 1 || info.PhotoBytes != 9 {
		t.Errorf("info = %+v", info)
	}

	var st Stats
	_, body = get(t, srv, "/api/stats?type=Transformer&status=Not+Inspected")
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.Total != 1 || st.NotInspected != 1 {
		t.Errorf("stats = %+v", st)
	}

	// A new download of the share is picked up without a restart.
	os.WriteFile(path, []byte(`{"type":"FeatureCollection","features":[]}`), 0o644)
	_, body = get(t, srv, "/api/info")
	json.Unmarshal(body, &info)
	if info.Features != 0 {
		t.Errorf("after rewrite: %d features", info.Features)
	}
}

func TestServerDownloads(t *testing.T) {
	srv, path := newServer(t, true)
	want, _ := os.ReadFile(path)

	resp, body := get(t, srv, "/download/geojson")
	if resp.StatusCode != 200 || !bytes.Equal(body, want) {
		t.Errorf("geojson: status %d, %d bytes", resp.StatusCode, len(body))
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="tok.geojson"` {
		t.Errorf("geojson disposition = %q", cd)
	}

	resp, body = get(t, srv, "/download/zip")
	if resp.StatusCode != 200 {
		t.Fatalf("zip: status %d", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="tok-with-photos.zip"` {
		t.Errorf("zip disposition = %q", cd)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(data)
	}
	if len(got) != 2 || got["tok.geojson"] != string(want) || got["photos/"+photoA+".jpg"] != "picture a" {
		t.Errorf("zip entries = %v", reflect.ValueOf(got).MapKeys())
	}
}

func TestServerWithoutPhotosOrData(t *testing.T) {
	srv, path := newServer(t, false)
	if resp, _ := get(t, srv, "/download/zip"); resp.StatusCode != 404 {
		t.Errorf("zip without photos: %d", resp.StatusCode)
	}

	os.Remove(path)
	resp, body := get(t, srv, "/api/info")
	if resp.StatusCode != 500 || !strings.Contains(string(body), "run fulcrum") {
		t.Errorf("missing geojson: %d %s", resp.StatusCode, body)
	}
	if resp, _ := get(t, srv, "/download/geojson"); resp.StatusCode != 404 {
		t.Errorf("geojson download without file: %d", resp.StatusCode)
	}
}
