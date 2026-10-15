package buildlogs

import (
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Filter selects stored events without changing their sequence or live cursor.
type Filter struct{ Source, Query string }

func parseFilter(values url.Values) (Filter, error) {
	f := Filter{Source: values.Get("source"), Query: values.Get("q")}
	switch f.Source {
	case "", "all", "previa", "application":
	default:
		return Filter{}, errors.New("invalid log source")
	}
	if utf8.RuneCountInString(f.Query) > 200 || strings.ContainsRune(f.Query, 0) {
		return Filter{}, errors.New("invalid log search")
	}
	return f, nil
}

func (f Filter) kind() string {
	switch f.Source {
	case "previa":
		return kindSystem
	case "application":
		return kindOutput
	}
	return ""
}

// Matches uses literal, case-insensitive text, like the database search.
func (f Filter) Matches(l Line) bool {
	return (f.Source != "previa" || l.System) && (f.Source != "application" || !l.System) && strings.Contains(strings.ToLower(l.Text), strings.ToLower(f.Query))
}
