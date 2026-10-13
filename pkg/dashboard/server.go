package dashboard

import (
	"archive/zip"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
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

	// PhotosReferenced is the number of photos the records link to;
	// PhotosOnDisk and PhotoBytes are the number and total size of those
	// that have been downloaded.
	PhotosReferenced int   `json:"photosReferenced"`
	PhotosOnDisk     int   `json:"photosOnDisk"`
	PhotoBytes       int64 `json:"photoBytes"`

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
//	GET /download/zip       a zip archive of the GeoJSON and the photos
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.servePage)
	mux.HandleFunc("GET /api/info", s.serveInfo)
	mux.HandleFunc("GET /api/stats", s.serveStats)
	mux.HandleFunc("GET /download/geojson", s.serveGeoJSON)
	mux.HandleFunc("GET /download/zip", s.serveZip)
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
	for _, p := range s.photoFiles() {
		info.PhotosOnDisk++
		info.PhotoBytes += p.Size()
	}
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

// serveZip streams a zip archive holding the GeoJSON and, in a "photos"
// folder, every downloaded photo. The archive is written as it is sent, so
// its size is not known in advance. The photos are stored uncompressed,
// since JPEG data does not compress.
func (s *Server) serveZip(w http.ResponseWriter, r *http.Request) {
	st, err := os.Stat(s.GeoJSONPath)
	if err != nil {
		http.Error(w, "the GeoJSON has not been downloaded yet", http.StatusNotFound)
		return
	}
	pictures := s.photoFiles()
	if len(pictures) == 0 {
		http.Error(w, "no photos have been downloaded yet: run fulcrum -photos to fetch them", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", attachment(s.Name+"-with-photos.zip"))
	zw := zip.NewWriter(w)
	// Once the first byte is out the status cannot change, so a failure can
	// only cut the download short; the browser then reports it as failed.
	if err := addFile(zw, s.Name+".geojson", s.GeoJSONPath, st.ModTime(), zip.Deflate); err != nil {
		return
	}
	for _, p := range pictures {
		if r.Context().Err() != nil {
			return
		}
		path := filepath.Join(s.PhotosDir, p.Name())
		if err := addFile(zw, "photos/"+p.Name(), path, p.ModTime(), zip.Store); err != nil {
			return
		}
	}
	zw.Close()
}

// addFile copies the file at path into the archive under name.
func addFile(zw *zip.Writer, name, path string, modified time.Time, method uint16) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	entry, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: modified})
	if err != nil {
		return err
	}
	_, err = io.Copy(entry, file)
	return err
}

// photoFiles returns the downloaded photos in PhotosDir, in name order.
// Unfinished downloads and subdirectories are left out. It returns nil if
// the directory does not exist.
func (s *Server) photoFiles() []fs.FileInfo {
	entries, err := os.ReadDir(s.PhotosDir)
	if err != nil {
		return nil
	}
	var out []fs.FileInfo
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), partSuffix) {
			continue
		}
		if info, err := e.Info(); err == nil {
			out = append(out, info)
		}
	}
	return out
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
