// Package geojson holds the minimal GeoJSON types needed to read a Fulcrum
// data share.
//
// It is not a general GeoJSON library. It decodes a FeatureCollection as a
// stream, keeps the properties of each feature in document order, and leaves
// geometry coordinates undecoded until a caller needs them:
//
//	err := geojson.DecodeFeatures(r, func(raw json.RawMessage, f *geojson.Feature) error {
//		id, _ := f.Properties.Get("fulcrum_id")
//		...
//		return nil
//	})
package geojson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Feature is a single GeoJSON feature: one Fulcrum record.
type Feature struct {
	// Type is the GeoJSON object type, always "Feature".
	Type string `json:"type"`

	// Properties holds the record's field values in document order.
	Properties Properties `json:"properties"`

	// Geometry is the record's location. It is nil for a record without one.
	Geometry *Geometry `json:"geometry"`
}

// Geometry is a GeoJSON geometry whose coordinates are decoded on demand,
// because their nesting depends on Type.
type Geometry struct {
	// Type is the GeoJSON geometry type, such as "Point" or "Polygon".
	Type string `json:"type"`

	// Coordinates is the undecoded coordinates array: a position for a
	// Point, an array of positions for a LineString, and so on.
	Coordinates json.RawMessage `json:"coordinates"`
}

// Property is one key/value pair of a feature.
type Property struct {
	// Key is the property name.
	Key string

	// Value is nil, a string, a json.Number, a bool, or (for nested arrays
	// and objects) whatever encoding/json produces for an interface value.
	Value any
}

// Properties keeps the properties of a feature in document order, so the
// shapefile columns come out in the same order as the share's fields.
type Properties []Property

// Get returns the value stored under key and whether the key is present. A
// present key may still hold a nil value (JSON null).
func (p Properties) Get(key string) (any, bool) {
	for _, prop := range p {
		if prop.Key == key {
			return prop.Value, true
		}
	}
	return nil, false
}

// UnmarshalJSON implements json.Unmarshaler. It decodes a JSON object,
// preserving key order and keeping numbers as json.Number so integers are
// not rounded through float64. A JSON null yields nil Properties.
func (p *Properties) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		*p = nil
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("properties is not a JSON object")
	}
	out := make(Properties, 0, 16)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("unexpected property key %v", tok)
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			return err
		}
		out = append(out, Property{Key: key, Value: v})
	}
	*p = out
	return nil
}

// DecodeFeatures streams a FeatureCollection from r, calling fn for every
// feature with both its original JSON and its decoded form. The whole
// document is never held in memory at once.
//
// Members of the collection other than "features" are ignored. Decoding stops
// at the first error, including one returned by fn, and that error is
// returned. A document without a "features" array is an error.
func DecodeFeatures(r io.Reader, fn func(raw json.RawMessage, f *Feature) error) error {
	dec := json.NewDecoder(r)
	if err := expectDelim(dec, '{'); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("empty response body")
		}
		return err
	}
	found := false
	n := 0
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if key, _ := tok.(string); key != "features" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return err
			}
			continue
		}
		found = true
		if err := expectDelim(dec, '['); err != nil {
			return err
		}
		for dec.More() {
			// A fresh RawMessage per feature: Decode reuses the backing
			// array of the one it is given.
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return err
			}
			n++
			f := new(Feature)
			if err := json.Unmarshal(raw, f); err != nil {
				return fmt.Errorf("feature %d: %w", n, err)
			}
			if err := fn(raw, f); err != nil {
				return err
			}
		}
		if err := expectDelim(dec, ']'); err != nil {
			return err
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return err
	}
	if !found {
		return errors.New(`not a GeoJSON FeatureCollection: no "features" array`)
	}
	return nil
}

// expectDelim reads the next token from dec and reports an error unless it
// is the delimiter want. A read error, including io.EOF, is returned as is.
func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("invalid GeoJSON: expected %q, got %v", want, tok)
	}
	return nil
}
