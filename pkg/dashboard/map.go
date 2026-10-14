package dashboard

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/mikey1964x/fulcrum/pkg/geojson"
)

// mapPage is the map of the share's units. Unlike the dashboard page it
// needs the internet, for the Leaflet library and the base map tiles.
//
//go:embed static/map.html
var mapPage []byte

// buildLayer returns the GeoJSON the map draws, plain and gzipped. The share
// is large and mostly attributes, so the layer holds only the geometry of
// each feature and the two properties that style it: "s", its status, and
// "c", its marker color. The id of a feature is its position in features,
// which serveFeature uses to look up the rest. Features without a geometry
// are left out.
func buildLayer(features []*geojson.Feature) (plain, gzipped []byte, err error) {
	var buf bytes.Buffer
	buf.WriteString(`{"type":"FeatureCollection","features":[`)
	first := true
	for i, f := range features {
		if f.Geometry == nil || len(f.Geometry.Coordinates) == 0 {
			continue
		}
		props, err := json.Marshal(map[string]string{"s": text(f, "status"), "c": text(f, "marker-color")})
		if err != nil {
			return nil, nil, err
		}
		geom, err := json.Marshal(f.Geometry)
		if err != nil {
			return nil, nil, err
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		fmt.Fprintf(&buf, `{"type":"Feature","id":%d,"properties":%s,"geometry":%s}`, i, props, geom)
	}
	buf.WriteString("]}")

	var gz bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if err != nil {
		return nil, nil, err
	}
	if _, err := zw.Write(buf.Bytes()); err != nil {
		return nil, nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), gz.Bytes(), nil
}

// propertiesJSON encodes properties as a JSON object, keeping their order.
func propertiesJSON(properties geojson.Properties) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, p := range properties {
		key, err := json.Marshal(p.Key)
		if err != nil {
			return nil, err
		}
		value, err := json.Marshal(p.Value)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// serveMapPage serves the map page.
func (s *Server) serveMapPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(mapPage)
}

// serveFeatures serves the layer the map draws.
func (s *Server) serveFeatures(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Load(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	body, gzipped, modTime := s.layer, s.layerGzip, s.modTime
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/geo+json")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Vary", "Accept-Encoding")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		body = gzipped
	}
	http.ServeContent(w, r, "", modTime, bytes.NewReader(body))
}

// serveFeature serves every property of one feature, for the map's popup.
func (s *Server) serveFeature(w http.ResponseWriter, r *http.Request) {
	features, err := s.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 0 || id >= len(features) {
		http.NotFound(w, r)
		return
	}
	body, err := propertiesJSON(features[id].Properties)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}
