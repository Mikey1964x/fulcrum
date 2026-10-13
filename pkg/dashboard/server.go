package dashboard

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mikey1964x/fulcrum/pkg/geojson"
	"github.com/mikey1964x/fulcrum/pkg/photos"
)

// page is the dashboard: one HTML file with its styles and script inline, so
// it needs nothing from the internet.
//
//go:embed static/index.html
var page []byte

// partSuffix is the extension of a photo that is still being downloaded.
const partSuffix = ".part"

// Server serves the dashboard of one downloaded share. The zero value is not
// usable; set the fields and call Handler.
//
// The GeoJSON is read when first needed and again whenever the file changes,
// so a new download by the fulcrum command shows up without a restart.
type Server struct {
	// Name is the base name of the share's files, used to name downloads.
	Name string

	// GeoJSONPath is the GeoJSON file saved by the fulcrum command.
	GeoJSONPath string

	// PhotosDir is the directory holding the downloaded photos. It need not
	// exist.
	PhotosDir string

	// ShareURL is the link of the data share, used to count the photos the
	// records refer to.
	ShareURL string

	mu       sync.Mutex
	modTime  time.Time
	size     int64
	features []*geojson.Feature
	options  Options
	photoRef int
}

// Info describes the data behind the dashboard.
type Info struct {
	// Name is the base name of the share's files.
	Name string `json:"name"`

	// Updated is when the GeoJSON was downloaded and GeoJSONBytes its size.
	Updated      time.Time `json:"updated"`
	GeoJSONBytes int64     `json:"geojsonBytes"`

	// Features is the number of records in the share.
	Features int `json:"features"`

	// PhotosReferenced is the number of photos the records link to and
	// PhotosOnDisk the number of those that have been downloaded.
	PhotosReferenced int `json:"photosReferenced"`
	PhotosOnDisk     int `json:"photosOnDisk"`

	// Options lists the values the statistics can be filtered on.
	Options Options `json:"options"`
}

// Handler returns the handler serving the dashboard:
//
//	GET /                   the page
//	GET /api/info           an Info as JSON
//	GET /api/stats          a Stats as JSON; the query parameters type,
//	                        county and status filter the records
//	GET /download/geojson   the GeoJSON file
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.servePage)
	mux.HandleFunc("GET /api/info", s.serveInfo)
	mux.HandleFunc("GET /api/stats", s.serveStats)
	mux.HandleFunc("GET /download/geojson", s.serveGeoJSON)
	return mux
}

// Load reads the GeoJSON if it has not been read yet or has changed on disk,
// and returns its features. The returned slice must not be modified.
func (s *Server) Load() ([]*geojson.Feature, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, err := os.Stat(s.GeoJSONPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s not found: run fulcrum without -serve first to download the share", s.GeoJSONPath)
	}
	if err != nil {
		return nil, err
	}
	if s.features != nil && st.ModTime().Equal(s.modTime) && st.Size() == s.size {
		return s.features, nil
	}

	file, err := os.Open(s.GeoJSONPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	features := []*geojson.Feature{}
	err = geojson.DecodeFeatures(file, func(_ json.RawMessage, f *geojson.Feature) error {
		features = append(features, f)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.GeoJSONPath, err)
	}

	refs, _ := photos.Collect(features, s.ShareURL)
	s.features, s.options, s.photoRef = features, ListOptions(features), len(refs)
	s.modTime, s.size = st.ModTime(), st.Size()
	return features, nil
}

// servePage serves the dashboard page.
func (s *Server) servePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(page)
}

// serveInfo serves the Info of the share.
func (s *Server) serveInfo(w http.ResponseWriter, r *http.Request) {
	features, err := s.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	info := Info{
		Name:             s.Name,
		Updated:          s.modTime,
		GeoJSONBytes:     s.size,
		Features:         len(features),
		PhotosReferenced: s.photoRef,
		Options:          s.options,
	}
	s.mu.Unlock()
	info.PhotosOnDisk = s.photoCount()
	writeJSON(w, info)
}

// serveStats serves the Stats of the records selected by the query.
func (s *Server) serveStats(w http.ResponseWriter, r *http.Request) {
	features, err := s.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	writeJSON(w, Compute(features, Filter{
		Type:   q.Get("type"),
		County: q.Get("county"),
		Status: q.Get("status"),
	}))
}

// serveGeoJSON serves the GeoJSON file as a download.
func (s *Server) serveGeoJSON(w http.ResponseWriter, r *http.Request) {
	file, err := os.Open(s.GeoJSONPath)
	if err != nil {
		http.Error(w, "the GeoJSON has not been downloaded yet", http.StatusNotFound)
		return
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/geo+json")
	w.Header().Set("Content-Disposition", attachment(s.Name+".geojson"))
	http.ServeContent(w, r, "", st.ModTime(), file)
}

// photoCount returns the number of downloaded photos in PhotosDir.
// Unfinished downloads and subdirectories are not counted. It returns 0 if
// the directory does not exist.
func (s *Server) photoCount() int {
	entries, err := os.ReadDir(s.PhotosDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && !strings.HasSuffix(e.Name(), partSuffix) {
			n++
		}
	}
	return n
}

// attachment returns a Content-Disposition value that makes the browser
// save the response under filename.
func attachment(filename string) string {
	return fmt.Sprintf("attachment; filename=%q", filename)
}

// writeJSON sends v as a JSON response that is not cached.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
