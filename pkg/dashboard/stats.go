// Package dashboard serves a web page with the statistics of a downloaded
// data share: how many units have been inspected, what the inspections found,
// and a link to download the GeoJSON.
//
// The statistics are computed by [Compute] from the features of the share.
// A [Server] reads the GeoJSON that the fulcrum command saved and serves the
// page, the statistics as JSON and the downloads.
package dashboard

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mikey1964x/fulcrum/pkg/geojson"
)

// Names of the share's attributes the statistics are computed from.
const (
	propStatus  = "status"
	propType    = "inspection_type"
	propCounty  = "address_sub_admin_area"
	propDate    = "inspection_date"
	propUpdated = "updated_at"
)

// Values of the status attribute that count as inspected and not inspected.
// Every other status, such as "Not In Field", counts as other.
const (
	statusInspected    = "Inspection Complete"
	statusNotInspected = "Not Inspected"
)

// unknown labels the records that have no value for an attribute.
const unknown = "Unknown"

// conditions lists the findings of an inspection, in the order they are
// shown. Each is a choice attribute; some allow several choices.
var conditions = []struct{ key, title string }{
	{"exterior", "Exterior"},
	{"clearance_", "Clearance"},
	{"penta_bolt", "Penta bolt"},
	{"lock", "Lock"},
	{"label", "Label"},
	{"oil_leak", "Oil leak"},
	{"pad", "Pad"},
}

// Filter selects the records the statistics are computed over. An empty
// field matches every record.
type Filter struct {
	// Type is an inspection type, such as "Transformer".
	Type string

	// County is a county name, or "Unknown" for records without one.
	County string

	// Status is a record status, such as "Not Inspected".
	Status string

	// Year is a four-digit year, such as "2025". It selects the records
	// that were last updated in that year, by their updated_at attribute.
	Year string
}

// Count is the number of records with one value of an attribute.
type Count struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Split is the inspection progress of one group of records.
type Split struct {
	Label        string `json:"label"`
	Total        int    `json:"total"`
	Inspected    int    `json:"inspected"`
	NotInspected int    `json:"notInspected"`
	Other        int    `json:"other"`
}

// Field is the distribution of one inspection finding.
type Field struct {
	// Key is the attribute name and Title its display name.
	Key   string `json:"key"`
	Title string `json:"title"`

	// Answered is the number of records with a value and Blank the number
	// without one.
	Answered int `json:"answered"`
	Blank    int `json:"blank"`

	// Counts holds the number of records per choice, most frequent first.
	// A record with several choices is counted under each, so the counts
	// can add up to more than Answered.
	Counts []Count `json:"counts"`
}

// Stats is the summary of the records matching a Filter.
type Stats struct {
	// Total is the number of records; Inspected, NotInspected and Other
	// divide it by status.
	Total        int `json:"total"`
	Inspected    int `json:"inspected"`
	NotInspected int `json:"notInspected"`
	Other        int `json:"other"`

	// Statuses holds the number of records per status, most frequent first.
	Statuses []Count `json:"statuses"`

	// Types and Counties hold the progress per inspection type and per
	// county, largest first.
	Types    []Split `json:"types"`
	Counties []Split `json:"counties"`

	// Conditions holds the findings that have at least one value.
	Conditions []Field `json:"conditions"`

	// Months holds the number of records per month of the inspection date,
	// labelled "2024-05", for every month from the first to the last.
	Months []Count `json:"months"`
}

// Options lists the values a Filter can select.
type Options struct {
	Types    []string `json:"types"`
	Counties []string `json:"counties"`
	Statuses []string `json:"statuses"`

	// Years lists the years offered for Filter.Year, newest first. It is
	// filled in by the server from [Years], not by ListOptions.
	Years []string `json:"years"`
}

// text returns the trimmed string value of a property, or "" if it is
// missing, null or not a string.
func text(f *geojson.Feature, key string) string {
	v, _ := f.Properties.Get(key)
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// orUnknown returns s, or "Unknown" if it is empty.
func orUnknown(s string) string {
	if s == "" {
		return unknown
	}
	return s
}

// choices splits the value of a choice attribute into its choices.
//
// Fulcrum joins several choices with a bare comma and quotes a choice that
// contains one: Lock in Place,"Lock Missing, Replaced". A field that allows
// only one choice is written without quotes even then: Old Label, New Label
// added. So a comma separates choices only outside quotes and when no space
// follows it.
func choices(value string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if c := strings.TrimSpace(cur.String()); c != "" {
			out = append(out, c)
		}
		cur.Reset()
	}
	quoted := false
	for i := 0; i < len(value); i++ {
		switch c := value[i]; {
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted && i+1 < len(value) && value[i+1] != ' ':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// ListOptions returns the inspection types, counties and statuses that occur
// in features, each sorted by name.
func ListOptions(features []*geojson.Feature) Options {
	types, counties, statuses := map[string]int{}, map[string]int{}, map[string]int{}
	for _, f := range features {
		types[orUnknown(text(f, propType))]++
		counties[orUnknown(text(f, propCounty))]++
		statuses[orUnknown(text(f, propStatus))]++
	}
	return Options{Types: keys(types), Counties: keys(counties), Statuses: keys(statuses)}
}

// yearsOffered is the number of years the dashboard offers to filter on: the
// current year and the four before it.
const yearsOffered = 5

// Years returns the years a Filter can select at the time now: the current
// year and the four before it, newest first. The list follows the calendar,
// not the data, so a year without records is still offered.
func Years(now time.Time) []string {
	out := make([]string, yearsOffered)
	for i := range out {
		out[i] = strconv.Itoa(now.Year() - i)
	}
	return out
}

// updatedIn reports whether the record was last updated in year. Fulcrum
// writes updated_at as "2023-02-20 14:46:14 UTC", so the year is its first
// four characters.
func updatedIn(f *geojson.Feature, year string) bool {
	updated := text(f, propUpdated)
	return len(updated) > len(year) && strings.HasPrefix(updated, year) && updated[len(year)] == '-'
}

// keys returns the keys of m in alphabetical order.
func keys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Compute summarises the features that match filter.
func Compute(features []*geojson.Feature, filter Filter) *Stats {
	st := &Stats{}
	statuses := map[string]int{}
	types, counties := map[string]*Split{}, map[string]*Split{}
	months := map[string]int{}
	fields := make([]Field, len(conditions))
	choiceCounts := make([]map[string]int, len(conditions))
	for i, c := range conditions {
		fields[i] = Field{Key: c.key, Title: c.title}
		choiceCounts[i] = map[string]int{}
	}

	for _, f := range features {
		status := orUnknown(text(f, propStatus))
		typ := orUnknown(text(f, propType))
		county := orUnknown(text(f, propCounty))
		if filter.Type != "" && filter.Type != typ ||
			filter.County != "" && filter.County != county ||
			filter.Status != "" && filter.Status != status ||
			filter.Year != "" && !updatedIn(f, filter.Year) {
			continue
		}

		st.Total++
		statuses[status]++
		for _, group := range []struct {
			m     map[string]*Split
			label string
		}{{types, typ}, {counties, county}} {
			sp := group.m[group.label]
			if sp == nil {
				sp = &Split{Label: group.label}
				group.m[group.label] = sp
			}
			sp.Total++
			switch status {
			case statusInspected:
				sp.Inspected++
			case statusNotInspected:
				sp.NotInspected++
			default:
				sp.Other++
			}
		}
		switch status {
		case statusInspected:
			st.Inspected++
		case statusNotInspected:
			st.NotInspected++
		default:
			st.Other++
		}

		for i, c := range conditions {
			value := text(f, c.key)
			if value == "" {
				fields[i].Blank++
				continue
			}
			fields[i].Answered++
			for _, choice := range choices(value) {
				choiceCounts[i][choice]++
			}
		}

		if date := text(f, propDate); isDate(date) {
			months[date[:7]]++
		}
	}

	st.Statuses = sortedCounts(statuses)
	st.Types = sortedSplits(types)
	st.Counties = sortedSplits(counties)
	st.Conditions = []Field{}
	for i, field := range fields {
		if field.Answered == 0 {
			continue
		}
		field.Counts = sortedCounts(choiceCounts[i])
		st.Conditions = append(st.Conditions, field)
	}
	st.Months = monthSeries(months)
	return st
}

// isDate reports whether s looks like a date written as 2024-05-28.
func isDate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, c := range s {
		if i != 4 && i != 7 && (c < '0' || c > '9') {
			return false
		}
	}
	return s[5:7] >= "01" && s[5:7] <= "12"
}

// sortedCounts returns m as a list, largest count first and alphabetical
// within a count.
func sortedCounts(m map[string]int) []Count {
	out := make([]Count, 0, len(m))
	for label, n := range m {
		out = append(out, Count{label, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// sortedSplits returns m as a list, largest total first and alphabetical
// within a total.
func sortedSplits(m map[string]*Split) []Split {
	out := make([]Split, 0, len(m))
	for _, sp := range m {
		out = append(out, *sp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// monthSeries returns the counts of m, keyed "2024-05", as a list covering
// every month from the first to the last, so that months without records
// show as zero instead of being left out.
func monthSeries(m map[string]int) []Count {
	out := []Count{}
	if len(m) == 0 {
		return out
	}
	names := keys(m)
	first, last := names[0], names[len(names)-1]
	year, _ := strconv.Atoi(first[:4])
	month, _ := strconv.Atoi(first[5:7])
	for {
		label := fmt.Sprintf("%04d-%02d", year, month)
		out = append(out, Count{label, m[label]})
		if label >= last {
			return out
		}
		if month++; month > 12 {
			year, month = year+1, 1
		}
	}
}
