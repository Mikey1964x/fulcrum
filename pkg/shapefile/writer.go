// Package shapefile converts GeoJSON features into Esri shapefiles.
package shapefile

import (
	"bufio"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mikey1964x/fulcrum/pkg/geojson"
)

// Shape type codes of the shapefile format.
const (
	shpPoint      int32 = 1
	shpPolyLine   int32 = 3
	shpPolygon    int32 = 5
	shpMultiPoint int32 = 8
)

const (
	shpHeaderLen  = 100
	maxDBFFields  = 255
	maxDBFRecord  = 65535
	maxShpFileLen = math.MaxInt32 * 2 // the header stores the length in 16-bit words
)

// wgs84 is the coordinate system GeoJSON mandates, in the WKT form ArcGIS
// writes to .prj files.
const wgs84 = `GEOGCS["GCS_WGS_1984",DATUM["D_WGS_1984",SPHEROID["WGS_1984",6378137.0,298.257223563]],PRIMEM["Greenwich",0.0],UNIT["Degree",0.0174532925199433]]`

// Output describes one shapefile that was written.
type Output struct {
	Path     string // path of the .shp file
	Geometry string
	Features int
}

// Summary is the result of a conversion.
type Summary struct {
	Outputs   []Output
	Fields    []Field
	FieldMap  string // path of the CSV mapping DBF names to property names
	Skipped   int    // features without a usable geometry
	Truncated int    // attribute values cut to fit their column
}

type shape struct {
	points  [][2]float64
	parts   []int32
	invalid bool // a position had fewer than two ordinates
}

type group struct {
	suffix    string
	label     string
	shapeType int32
	features  []*geojson.Feature
	shapes    []shape
}

// Write converts features into shapefiles under dir. A shapefile holds a
// single geometry type, so when the features mix types one file per type is
// written (<name>_point, <name>_line, ...); otherwise the file is just <name>.
func Write(features []*geojson.Feature, dir, name string) (*Summary, error) {
	groups := []*group{
		{suffix: "point", label: "Point", shapeType: shpPoint},
		{suffix: "multipoint", label: "MultiPoint", shapeType: shpMultiPoint},
		{suffix: "line", label: "PolyLine", shapeType: shpPolyLine},
		{suffix: "polygon", label: "Polygon", shapeType: shpPolygon},
	}
	byType := make(map[int32]*group)
	for _, g := range groups {
		byType[g.shapeType] = g
	}

	sum := &Summary{}
	used := 0
	for _, f := range features {
		shapeType, s, err := toShape(f.Geometry)
		if err != nil {
			sum.Skipped++
			continue
		}
		g := byType[shapeType]
		if len(g.features) == 0 {
			used++
		}
		g.features = append(g.features, f)
		g.shapes = append(g.shapes, s)
	}
	if used == 0 {
		return nil, errors.New("no features with a usable geometry to write")
	}

	sum.Fields = InferFields(features)
	if len(sum.Fields) > maxDBFFields {
		return nil, fmt.Errorf("the share has %d fields; a shapefile table holds at most %d", len(sum.Fields), maxDBFFields)
	}

	for _, g := range groups {
		if len(g.features) == 0 {
			continue
		}
		base := filepath.Join(dir, name)
		if used > 1 {
			base += "_" + g.suffix
		}
		if err := writeShapes(base, g.shapeType, g.shapes); err != nil {
			return nil, err
		}
		truncated, err := writeDBF(base+".dbf", sum.Fields, g.features)
		if err != nil {
			return nil, err
		}
		sum.Truncated += truncated
		if err := os.WriteFile(base+".prj", []byte(wgs84), 0o644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(base+".cpg", []byte("UTF-8"), 0o644); err != nil {
			return nil, err
		}
		sum.Outputs = append(sum.Outputs, Output{Path: base + ".shp", Geometry: g.label, Features: len(g.features)})
	}

	sum.FieldMap = filepath.Join(dir, name+"_fields.csv")
	if err := writeFieldMap(sum.FieldMap, sum.Fields); err != nil {
		return nil, err
	}
	return sum, nil
}

// toShape converts a GeoJSON geometry to shapefile parts and points. Only
// the first two ordinates of a position are used.
func toShape(g *geojson.Geometry) (int32, shape, error) {
	if g == nil {
		return 0, shape{}, errors.New("no geometry")
	}
	var s shape
	var shapeType int32
	switch g.Type {
	case "Point":
		var c []float64
		if err := json.Unmarshal(g.Coordinates, &c); err != nil {
			return 0, s, err
		}
		shapeType = shpPoint
		s.addPart([][]float64{c})
	case "MultiPoint":
		var c [][]float64
		if err := json.Unmarshal(g.Coordinates, &c); err != nil {
			return 0, s, err
		}
		shapeType = shpMultiPoint
		s.addPart(c)
	case "LineString":
		var c [][]float64
		if err := json.Unmarshal(g.Coordinates, &c); err != nil {
			return 0, s, err
		}
		shapeType = shpPolyLine
		s.addPart(c)
	case "MultiLineString":
		var c [][][]float64
		if err := json.Unmarshal(g.Coordinates, &c); err != nil {
			return 0, s, err
		}
		shapeType = shpPolyLine
		for _, line := range c {
			s.addPart(line)
		}
	case "Polygon":
		var c [][][]float64
		if err := json.Unmarshal(g.Coordinates, &c); err != nil {
			return 0, s, err
		}
		shapeType = shpPolygon
		s.addPolygon(c)
	case "MultiPolygon":
		var c [][][][]float64
		if err := json.Unmarshal(g.Coordinates, &c); err != nil {
			return 0, s, err
		}
		shapeType = shpPolygon
		for _, poly := range c {
			s.addPolygon(poly)
		}
	default:
		return 0, s, fmt.Errorf("unsupported geometry type %q", g.Type)
	}
	if s.invalid || len(s.points) == 0 {
		return 0, shape{}, errors.New("geometry has no usable coordinates")
	}
	return shapeType, s, nil
}

func (s *shape) addPart(positions [][]float64) {
	if len(positions) == 0 {
		return
	}
	s.parts = append(s.parts, int32(len(s.points)))
	for _, p := range positions {
		if len(p) < 2 || math.IsNaN(p[0]) || math.IsNaN(p[1]) {
			s.invalid = true
			return
		}
		s.points = append(s.points, [2]float64{p[0], p[1]})
	}
}

// addPolygon adds the rings of one GeoJSON polygon in shapefile winding:
// the outer ring clockwise and holes counter-clockwise, each ring closed.
func (s *shape) addPolygon(rings [][][]float64) {
	for i, ring := range rings {
		if len(ring) < 3 {
			continue
		}
		ring = append([][]float64(nil), ring...)
		first, last := ring[0], ring[len(ring)-1]
		if len(first) >= 2 && len(last) >= 2 && (first[0] != last[0] || first[1] != last[1]) {
			ring = append(ring, first)
		}
		clockwise := signedArea(ring) < 0
		if hole := i > 0; clockwise == hole {
			for a, b := 0, len(ring)-1; a < b; a, b = a+1, b-1 {
				ring[a], ring[b] = ring[b], ring[a]
			}
		}
		s.addPart(ring)
	}
}

// signedArea is positive for a counter-clockwise ring.
func signedArea(ring [][]float64) float64 {
	var sum float64
	for i := 0; i+1 < len(ring); i++ {
		if len(ring[i]) < 2 || len(ring[i+1]) < 2 {
			return 0
		}
		sum += ring[i][0]*ring[i+1][1] - ring[i+1][0]*ring[i][1]
	}
	return sum / 2
}

type bbox struct{ minX, minY, maxX, maxY float64 }

func (b *bbox) extend(p [2]float64) {
	b.minX, b.maxX = math.Min(b.minX, p[0]), math.Max(b.maxX, p[0])
	b.minY, b.maxY = math.Min(b.minY, p[1]), math.Max(b.maxY, p[1])
}

func emptyBBox() bbox {
	return bbox{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
}

func (b bbox) append(buf []byte) []byte {
	for _, v := range [...]float64{b.minX, b.minY, b.maxX, b.maxY} {
		buf = appendFloat(buf, v)
	}
	return buf
}

func appendFloat(buf []byte, v float64) []byte {
	return binary.LittleEndian.AppendUint64(buf, math.Float64bits(v))
}

// appendContent appends the record content of s, without the record header.
func appendContent(buf []byte, shapeType int32, s shape) []byte {
	buf = binary.LittleEndian.AppendUint32(buf, uint32(shapeType))
	if shapeType == shpPoint {
		buf = appendFloat(buf, s.points[0][0])
		return appendFloat(buf, s.points[0][1])
	}
	box := emptyBBox()
	for _, p := range s.points {
		box.extend(p)
	}
	buf = box.append(buf)
	if shapeType != shpMultiPoint {
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(s.parts)))
	}
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(s.points)))
	if shapeType != shpMultiPoint {
		for _, part := range s.parts {
			buf = binary.LittleEndian.AppendUint32(buf, uint32(part))
		}
	}
	for _, p := range s.points {
		buf = appendFloat(buf, p[0])
		buf = appendFloat(buf, p[1])
	}
	return buf
}

// writeShapes writes the geometry file (.shp) and its index (.shx).
func writeShapes(base string, shapeType int32, shapes []shape) (err error) {
	shp, err := os.Create(base + ".shp")
	if err != nil {
		return err
	}
	defer closeFile(shp, &err)
	shx, err := os.Create(base + ".shx")
	if err != nil {
		return err
	}
	defer closeFile(shx, &err)

	shpW, shxW := bufio.NewWriter(shp), bufio.NewWriter(shx)
	// The headers need the final length and extent; reserve their space now.
	shpW.Write(make([]byte, shpHeaderLen))
	shxW.Write(make([]byte, shpHeaderLen))

	box := emptyBBox()
	offset := int64(shpHeaderLen)
	var content, rec []byte
	for i, s := range shapes {
		for _, p := range s.points {
			box.extend(p)
		}
		content = appendContent(content[:0], shapeType, s)
		words := uint32(len(content) / 2)

		rec = binary.BigEndian.AppendUint32(rec[:0], uint32(i+1))
		rec = binary.BigEndian.AppendUint32(rec, words)
		shpW.Write(rec)
		shpW.Write(content)

		rec = binary.BigEndian.AppendUint32(rec[:0], uint32(offset/2))
		rec = binary.BigEndian.AppendUint32(rec, words)
		shxW.Write(rec)

		offset += int64(8 + len(content))
		if offset > maxShpFileLen {
			return fmt.Errorf("%s.shp would exceed the shapefile size limit", base)
		}
	}
	if err := shpW.Flush(); err != nil {
		return err
	}
	if err := shxW.Flush(); err != nil {
		return err
	}
	if _, err := shp.WriteAt(shpHeader(shapeType, offset, box), 0); err != nil {
		return err
	}
	_, err = shx.WriteAt(shpHeader(shapeType, int64(shpHeaderLen+8*len(shapes)), box), 0)
	return err
}

func shpHeader(shapeType int32, fileLen int64, box bbox) []byte {
	h := make([]byte, 0, shpHeaderLen)
	h = binary.BigEndian.AppendUint32(h, 9994)
	h = append(h, make([]byte, 20)...)
	h = binary.BigEndian.AppendUint32(h, uint32(fileLen/2))
	h = binary.LittleEndian.AppendUint32(h, 1000)
	h = binary.LittleEndian.AppendUint32(h, uint32(shapeType))
	h = box.append(h)
	return append(h, make([]byte, 32)...) // Z and M ranges, unused
}

// writeDBF writes the attribute table and returns how many values were
// truncated to fit their column.
func writeDBF(path string, fields []Field, features []*geojson.Feature) (truncated int, err error) {
	recordLen := 1
	offsets := make(map[string]int, len(fields))
	index := make(map[string]int, len(fields))
	for i, f := range fields {
		offsets[f.Source] = recordLen
		index[f.Source] = i
		recordLen += f.Width
	}
	if recordLen > maxDBFRecord {
		return 0, fmt.Errorf("the attribute record is %d bytes; a shapefile table allows at most %d", recordLen, maxDBFRecord)
	}
	headerLen := 32 + 32*len(fields) + 1

	file, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer closeFile(file, &err)
	w := bufio.NewWriter(file)

	now := time.Now()
	h := []byte{0x03, byte(now.Year() - 1900), byte(now.Month()), byte(now.Day())}
	h = binary.LittleEndian.AppendUint32(h, uint32(len(features)))
	h = binary.LittleEndian.AppendUint16(h, uint16(headerLen))
	h = binary.LittleEndian.AppendUint16(h, uint16(recordLen))
	h = append(h, make([]byte, 20)...)
	for _, f := range fields {
		d := make([]byte, 32)
		copy(d[:maxNameLen], f.Name)
		d[11] = 'C'
		if f.Type != Text {
			d[11] = 'N'
		}
		d[16] = byte(f.Width)
		d[17] = byte(f.Decimals)
		h = append(h, d...)
	}
	h = append(h, 0x0D)
	w.Write(h)

	rec := make([]byte, recordLen)
	for _, feat := range features {
		for i := range rec {
			rec[i] = ' '
		}
		for _, p := range feat.Properties {
			off, ok := offsets[p.Key]
			if !ok {
				continue
			}
			f := fields[index[p.Key]]
			s, cut := f.format(p.Value)
			if cut {
				truncated++
			}
			cell := rec[off : off+f.Width]
			for i := range cell {
				cell[i] = ' '
			}
			if f.Type != Text {
				cell = cell[f.Width-len(s):] // numbers are right-aligned
			}
			copy(cell, s)
		}
		w.Write(rec)
	}
	w.WriteByte(0x1A)
	return truncated, w.Flush()
}

func writeFieldMap(path string, fields []Field) (err error) {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer closeFile(file, &err)
	w := csv.NewWriter(file)
	w.Write([]string{"dbf_name", "source_property", "type", "width", "decimals"})
	for _, f := range fields {
		w.Write([]string{f.Name, f.Source, f.Type.String(), strconv.Itoa(f.Width), strconv.Itoa(f.Decimals)})
	}
	w.Flush()
	return w.Error()
}

// closeFile closes f and keeps the close error if nothing failed earlier.
func closeFile(f *os.File, err *error) {
	if cerr := f.Close(); cerr != nil && *err == nil {
		*err = cerr
	}
}
