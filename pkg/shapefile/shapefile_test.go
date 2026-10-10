package shapefile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	shp "github.com/jonas-p/go-shp"

	"github.com/mikey1964x/fulcrum/pkg/geojson"
)

// decode parses the FeatureCollection doc into features, failing the test on
// a decoding error.
func decode(t *testing.T, doc string) []*geojson.Feature {
	t.Helper()
	var feats []*geojson.Feature
	err := geojson.DecodeFeatures(strings.NewReader(doc), func(_ json.RawMessage, f *geojson.Feature) error {
		feats = append(feats, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return feats
}

// TestDBFNames checks column naming on property names taken from a real
// share: truncation, collision suffixes, case-insensitive uniqueness and
// sanitising of characters a DBF name cannot hold.
func TestDBFNames(t *testing.T) {
	keys := []string{
		"fulcrum_id", "xfmr_photo_caption", "xfmr_photo", "xfmr_photo_url",
		"marker-color", "address_sub_thoroughfare", "address_thoroughfare",
		"Status", "status", "2nd_reading", "clearance_", "clearance__other",
	}
	want := []string{
		"fulcrum_id", "xfmr_pho_1", "xfmr_photo", "xfmr_pho_2",
		"marker_col", "address_su", "address_th",
		"Status", "status_1", "f2nd_readi", "clearance_", "clearanc_1",
	}
	got := dbfNames(keys)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dbfNames:\n got %q\nwant %q", got, want)
	}
	seen := map[string]bool{}
	for _, n := range got {
		if len(n) > maxNameLen || seen[strings.ToUpper(n)] {
			t.Errorf("name %q is too long or not unique", n)
		}
		seen[strings.ToUpper(n)] = true
	}
}

// TestInferFields checks the inferred type and width of text, integer,
// decimal, mixed, all-null and boolean properties, and that a property first
// seen in a later feature still gets a column.
func TestInferFields(t *testing.T) {
	feats := decode(t, `{"type":"FeatureCollection","features":[
		{"type":"Feature","properties":{"id":"a","version":3,"lat":33.75698091,"mixed":1,"empty":null,"kva":"75","flag":true},"geometry":null},
		{"type":"Feature","properties":{"id":"bcd","version":-12,"lat":-8.5,"mixed":"x","empty":null,"kva":null,"flag":false,"late":9007199254740993},"geometry":null}
	]}`)
	got := InferFields(feats)
	want := []Field{
		{Name: "id", Source: "id", Type: Text, Width: 3},
		{Name: "version", Source: "version", Type: Integer, Width: 3},
		{Name: "lat", Source: "lat", Type: Float, Width: 11, Decimals: 8},
		{Name: "mixed", Source: "mixed", Type: Text, Width: 1},
		{Name: "empty", Source: "empty", Type: Text, Width: 1},
		{Name: "kva", Source: "kva", Type: Text, Width: 2},
		{Name: "flag", Source: "flag", Type: Text, Width: 5},
		{Name: "late", Source: "late", Type: Integer, Width: 16},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("InferFields:\n got %+v\nwant %+v", got, want)
	}
}

// TestTruncateKeepsRunesWhole checks that truncation never cuts a multi-byte
// UTF-8 character in half.
func TestTruncateKeepsRunesWhole(t *testing.T) {
	got, cut := truncate("abcé", 4) // é is two bytes
	if got != "abc" || !cut {
		t.Errorf("truncate = %q, %v; want \"abc\", true", got, cut)
	}
	if got, cut := truncate("abc", 3); got != "abc" || cut {
		t.Errorf("truncate = %q, %v; want \"abc\", false", got, cut)
	}
}

// mixedDoc is a collection with two points (one carrying a Z ordinate and an
// over-long text value), a line, a polygon with a hole, and a feature without
// geometry.
var mixedDoc = `{"type":"FeatureCollection","features":[
	{"type":"Feature","properties":{"name":"Café","count":7,"value":1.25,"note":null},"geometry":{"type":"Point","coordinates":[-78.5,33.25]}},
	{"type":"Feature","properties":{"name":"` + longText + `","count":-120,"value":10,"note":"n"},"geometry":{"type":"Point","coordinates":[-79,34,12.5]}},
	{"type":"Feature","properties":{"name":"line","count":1,"value":0.5,"note":null},"geometry":{"type":"LineString","coordinates":[[0,0],[1,1],[2,0]]}},
	{"type":"Feature","properties":{"name":"poly","count":2,"value":2,"note":null},"geometry":{"type":"Polygon","coordinates":[[[0,0],[4,0],[4,4],[0,4],[0,0]],[[1,1],[2,1],[2,2],[1,2],[1,1]]]}},
	{"type":"Feature","properties":{"name":"nowhere","count":3,"value":3,"note":null},"geometry":null}
]}`

// longText is a value longer than a DBF text column can hold.
var longText = strings.Repeat("x", 300)

// TestWriteMixedGeometries writes mixedDoc and reads the result back with an
// independent shapefile reader, checking the per-type files, geometries,
// attributes, truncation, skipped features and polygon ring winding.
func TestWriteMixedGeometries(t *testing.T) {
	dir := t.TempDir()
	sum, err := Write(decode(t, mixedDoc), dir, "t")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Skipped != 1 || sum.Truncated != 1 || len(sum.Outputs) != 3 {
		t.Fatalf("skipped %d, truncated %d, outputs %d; want 1, 1, 3", sum.Skipped, sum.Truncated, len(sum.Outputs))
	}
	for _, ext := range []string{".shp", ".shx", ".dbf", ".prj", ".cpg"} {
		for _, base := range []string{"t_point", "t_line", "t_polygon"} {
			if _, err := os.Stat(filepath.Join(dir, base+ext)); err != nil {
				t.Errorf("missing output: %v", err)
			}
		}
	}

	// Points, read back with an independent shapefile reader.
	r, err := shp.Open(filepath.Join(dir, "t_point.shp"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.GeometryType != shp.POINT {
		t.Errorf("geometry type %v, want POINT", r.GeometryType)
	}
	var names []string
	for _, f := range r.Fields() {
		names = append(names, f.String())
	}
	if want := []string{"name", "count", "value", "note"}; !reflect.DeepEqual(names, want) {
		t.Errorf("fields %q, want %q", names, want)
	}
	wantRows := [][]string{
		{"Café", "7", "1.25", ""},
		{strings.Repeat("x", 254), "-120", "10.00", "n"},
	}
	wantPts := []shp.Point{{X: -78.5, Y: 33.25}, {X: -79, Y: 34}}
	n := 0
	for r.Next() {
		i, s := r.Shape()
		if p, ok := s.(*shp.Point); !ok || *p != wantPts[i] {
			t.Errorf("shape %d = %v, want %v", i, s, wantPts[i])
		}
		for k := range names {
			if got := r.ReadAttribute(i, k); got != wantRows[i][k] {
				t.Errorf("row %d %s = %q, want %q", i, names[k], got, wantRows[i][k])
			}
		}
		n++
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 2 || r.AttributeCount() != 2 {
		t.Errorf("read %d shapes and %d rows, want 2 and 2", n, r.AttributeCount())
	}
	if box := r.BBox(); box.MinX != -79 || box.MaxX != -78.5 || box.MinY != 33.25 || box.MaxY != 34 {
		t.Errorf("bounding box %+v", box)
	}

	// Line.
	lr, err := shp.Open(filepath.Join(dir, "t_line.shp"))
	if err != nil {
		t.Fatal(err)
	}
	defer lr.Close()
	if !lr.Next() {
		t.Fatalf("no line read: %v", lr.Err())
	}
	_, s := lr.Shape()
	line, ok := s.(*shp.PolyLine)
	if !ok || line.NumParts != 1 || line.NumPoints != 3 || line.Points[2] != (shp.Point{X: 2, Y: 0}) {
		t.Errorf("line = %+v", s)
	}
	if got := lr.ReadAttribute(0, 0); got != "line" {
		t.Errorf("line name = %q", got)
	}

	// Polygon: outer ring must come out clockwise, the hole counter-clockwise.
	pr, err := shp.Open(filepath.Join(dir, "t_polygon.shp"))
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	if !pr.Next() {
		t.Fatalf("no polygon read: %v", pr.Err())
	}
	_, s = pr.Shape()
	poly, ok := s.(*shp.Polygon)
	if !ok || poly.NumParts != 2 || poly.NumPoints != 10 {
		t.Fatalf("polygon = %+v", s)
	}
	if a := ringArea(poly.Points[0:5]); a != -16 {
		t.Errorf("outer ring area %v, want -16 (clockwise)", a)
	}
	if a := ringArea(poly.Points[5:10]); a != 1 {
		t.Errorf("hole area %v, want 1 (counter-clockwise)", a)
	}
}

// ringArea returns the signed area of a closed ring: positive when it runs
// counter-clockwise, negative when clockwise.
func ringArea(pts []shp.Point) float64 {
	var sum float64
	for i := 0; i+1 < len(pts); i++ {
		sum += pts[i].X*pts[i+1].Y - pts[i+1].X*pts[i].Y
	}
	return sum / 2
}

// TestWriteSingleGeometryType checks that a collection of one geometry type
// is written without a type suffix, with a WGS 84 .prj and a field map that
// traces a renamed column.
func TestWriteSingleGeometryType(t *testing.T) {
	dir := t.TempDir()
	feats := decode(t, `{"type":"FeatureCollection","features":[
		{"type":"Feature","properties":{"xfmr_photo":"p","xfmr_photo_url":"u"},"geometry":{"type":"Point","coordinates":[1,2]}}
	]}`)
	sum, err := Write(feats, dir, "share")
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Outputs) != 1 || sum.Outputs[0].Path != filepath.Join(dir, "share.shp") {
		t.Errorf("outputs = %+v, want only share.shp", sum.Outputs)
	}
	prj, err := os.ReadFile(filepath.Join(dir, "share.prj"))
	if err != nil || !strings.Contains(string(prj), "GCS_WGS_1984") {
		t.Errorf("prj = %q, %v", prj, err)
	}
	csv, err := os.ReadFile(sum.FieldMap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(csv), "xfmr_pho_1,xfmr_photo_url,text,1,0") {
		t.Errorf("field map does not trace the renamed column:\n%s", csv)
	}
}

// TestWriteNothingUsable checks that Write fails rather than producing an
// empty shapefile when no feature has a geometry.
func TestWriteNothingUsable(t *testing.T) {
	feats := decode(t, `{"type":"FeatureCollection","features":[{"type":"Feature","properties":{"a":1},"geometry":null}]}`)
	if _, err := Write(feats, t.TempDir(), "x"); err == nil {
		t.Error("want an error when no feature has a geometry")
	}
}
