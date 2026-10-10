package shapefile

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mikey1964x/fulcrum/pkg/geojson"
)

// Limits of the dBASE table that carries a shapefile's attributes.
const (
	// maxNameLen is the longest column name, in bytes.
	maxNameLen = 10

	// maxTextWidth is the widest text column, in bytes.
	maxTextWidth = 254

	// maxIntWidth is the widest integer column; wider integers are stored
	// as text so no digits are lost.
	maxIntWidth = 18

	// maxFloatWidth is the widest decimal column, sign and point included.
	maxFloatWidth = 31

	// maxSignificant is the number of significant digits kept for decimal
	// values. A float64 carries 15 reliable ones; printing more only
	// exposes binary rounding noise (33.75698091 as 33.756980910000003).
	maxSignificant = 15
)

// FieldType is the kind of DBF column a property is stored in.
type FieldType int

// The column types the converter produces.
const (
	// Text is a character column ("C"), left-aligned and space-padded.
	Text FieldType = iota

	// Integer is a numeric column ("N") without decimals.
	Integer

	// Float is a numeric column ("N") with a fixed number of decimals.
	Float
)

// String returns the lower-case name of the type, as written to the field
// map CSV.
func (t FieldType) String() string {
	switch t {
	case Integer:
		return "integer"
	case Float:
		return "float"
	default:
		return "text"
	}
}

// Field describes one DBF column and the GeoJSON property it comes from.
type Field struct {
	// Name is the DBF column name: at most 10 characters and unique within
	// the table, ignoring case.
	Name string

	// Source is the original GeoJSON property name.
	Source string

	// Type is the kind of column.
	Type FieldType

	// Width is the column width in bytes.
	Width int

	// Decimals is the number of digits after the decimal point. It is zero
	// unless Type is Float.
	Decimals int
}

// fieldStats accumulates what InferFields has seen of one property across
// all features.
type fieldStats struct {
	seen     bool // at least one non-null value
	numeric  bool // every non-null value so far is a finite number
	integer  bool // every number so far is an integer literal
	textLen  int  // longest value as text, in bytes
	whole    int  // widest integer part, sign included
	digits   int  // most digits in an integer part
	decimals int  // most digits after the decimal point
}

// InferFields builds the table schema from the values actually present:
// columns appear in first-seen order, a column is numeric only if every
// non-null value is a number, and text columns are as wide as their longest
// value (up to the DBF limit).
//
// A property that is null in every feature becomes a text column of width 1.
// Booleans and nested arrays or objects are stored as text.
func InferFields(features []*geojson.Feature) []Field {
	var keys []string
	stats := make(map[string]*fieldStats)
	for _, f := range features {
		for _, p := range f.Properties {
			st, ok := stats[p.Key]
			if !ok {
				st = &fieldStats{numeric: true, integer: true}
				stats[p.Key] = st
				keys = append(keys, p.Key)
			}
			st.observe(p.Value)
		}
	}

	names := dbfNames(keys)
	fields := make([]Field, len(keys))
	for i, key := range keys {
		fields[i] = stats[key].field(names[i], key)
	}
	return fields
}

// observe folds one value of the property into the statistics. A nil value
// (JSON null) carries no information and is ignored.
func (st *fieldStats) observe(v any) {
	if v == nil {
		return
	}
	st.seen = true
	s := textOf(v)
	st.textLen = max(st.textLen, len(s))

	n, ok := v.(json.Number)
	if !ok {
		st.numeric = false
		return
	}
	if isIntLiteral(s) {
		st.whole = max(st.whole, len(s))
		return
	}
	st.integer = false
	f, err := n.Float64()
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		st.numeric = false
		return
	}
	whole, frac, _ := strings.Cut(strconv.FormatFloat(f, 'f', -1, 64), ".")
	st.whole = max(st.whole, len(whole))
	st.digits = max(st.digits, len(strings.TrimPrefix(whole, "-")))
	st.decimals = max(st.decimals, len(frac))
}

// field turns the statistics into the column definition for the property
// source, stored under the DBF name. Numbers too wide for a numeric column
// fall back to text.
func (st *fieldStats) field(name, source string) Field {
	fld := Field{Name: name, Source: source, Type: Text}
	decimals := min(st.decimals, max(maxSignificant-st.digits, 0))
	switch {
	case !st.seen:
		fld.Width = 1
	case st.numeric && st.integer && st.whole <= maxIntWidth:
		fld.Type = Integer
		fld.Width = st.whole
	case st.numeric && !st.integer && st.whole+1+decimals <= maxFloatWidth:
		fld.Type = Float
		fld.Decimals = decimals
		fld.Width = st.whole + 1 + decimals
	default:
		fld.Width = min(max(st.textLen, 1), maxTextWidth)
	}
	return fld
}

// format renders v for this column, without padding. A nil value renders as
// the empty string, which leaves the cell blank. The second result reports
// that the value did not fit and was cut.
func (f Field) format(v any) (string, bool) {
	if v == nil {
		return "", false
	}
	s := textOf(v)
	if f.Type == Float {
		if n, ok := v.(json.Number); ok {
			if x, err := n.Float64(); err == nil {
				s = strconv.FormatFloat(x, 'f', f.Decimals, 64)
			}
		}
	}
	return truncate(s, f.Width)
}

// textOf returns v as it is stored in a text column: strings unchanged,
// numbers as their JSON literal, booleans as "true" or "false", and arrays
// and objects as compact JSON.
func textOf(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// isIntLiteral reports whether the JSON number literal s is written as an
// integer, with no fraction or exponent.
func isIntLiteral(s string) bool {
	return s != "" && !strings.ContainsAny(s, ".eE")
}

// truncate cuts s to at most width bytes without splitting a UTF-8 sequence.
// It reports whether anything was cut.
func truncate(s string, width int) (string, bool) {
	if len(s) <= width {
		return s, false
	}
	for width > 0 && !utf8.RuneStart(s[width]) {
		width--
	}
	return s[:width], true
}

// dbfNames maps property names to unique DBF column names. Names that
// already fit are kept as they are; longer ones are truncated, and a
// truncation that collides gets a numeric suffix (xfmr_photo, xfmr_pho_1,
// xfmr_pho_2). Uniqueness is case-insensitive, as DBF readers require.
func dbfNames(keys []string) []string {
	names := make([]string, len(keys))
	used := make(map[string]bool)
	clean := make([]string, len(keys))

	// Reserve the names that fit first, so a long name never pushes a short
	// one off its own spelling.
	for i, key := range keys {
		clean[i] = sanitizeName(key)
		if len(clean[i]) <= maxNameLen && !used[strings.ToUpper(clean[i])] {
			names[i] = clean[i]
			used[strings.ToUpper(clean[i])] = true
		}
	}
	for i := range keys {
		if names[i] != "" {
			continue
		}
		base := clean[i][:min(len(clean[i]), maxNameLen)]
		name := base
		for n := 1; used[strings.ToUpper(name)]; n++ {
			suffix := "_" + strconv.Itoa(n)
			name = base[:min(len(base), maxNameLen-len(suffix))] + suffix
		}
		names[i] = name
		used[strings.ToUpper(name)] = true
	}
	return names
}

// sanitizeName makes key safe as a DBF column name, without shortening it:
// characters other than ASCII letters, digits and underscore become
// underscores, an empty name becomes "field", and a leading digit is
// prefixed with "f".
func sanitizeName(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if name == "" {
		return "field"
	}
	if name[0] >= '0' && name[0] <= '9' {
		name = "f" + name
	}
	return name
}
