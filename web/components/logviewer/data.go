// Package logviewer presents build output without depending on backend features.
package logviewer

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var timestampPrefix = regexp.MustCompile(`^\[?\d{4}[-/]\d{2}[-/]\d{2}[ T]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?\]?`)
var errorWord = regexp.MustCompile(`(?i)\b(error|failed|fatal|panic)\b`)

type Line struct {
	Sequence int64
	System   bool
	Text     string
	At       time.Time
}

func (l Line) Timestamp() string {
	if prefix := timestampPrefix.FindString(l.Text); prefix != "" {
		return prefix
	}
	if l.At.IsZero() {
		return "[timestamp unavailable]"
	}
	return l.At.UTC().Format("2006-01-02T15:04:05.000Z")
}
func (l Line) Message() string {
	if prefix := timestampPrefix.FindString(l.Text); prefix != "" {
		return strings.TrimPrefix(strings.TrimPrefix(l.Text, prefix), " ")
	}
	return l.Text
}
func (l Line) ISOTime() string {
	if l.At.IsZero() {
		return ""
	}
	return l.At.UTC().Format(time.RFC3339Nano)
}
func (l Line) Number() string { return strconv.FormatInt(l.Sequence+1, 10) }
func (l Line) Anchor() string { return "L" + l.Number() }
func (l Line) Source() string {
	if l.System {
		return "previa"
	}
	return "application"
}
func (l Line) IsError() bool { return !l.System && errorWord.MatchString(l.Text) }

type Panel struct {
	Lines                                        []Line
	Empty                                        string
	Source, Query                                string
	BaseURL, EventsURL, Cursor                   string
	EarlierURL, LaterURL, LatestURL, DownloadURL string
	FullURL                                      string
	Inline                                       bool
	HasOutput, CanDelete                         bool
}

func FilterURL(base, source, query string) string {
	values := url.Values{}
	if source != "" && source != "all" {
		values.Set("source", source)
	}
	if query != "" {
		values.Set("q", query)
	}
	if len(values) == 0 {
		return base
	}
	return base + "?" + values.Encode()
}
