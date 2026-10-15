package httpx

import (
	"encoding/json"
	"net/http"
	"strings"
)

// GitHubAvatarOrigin serves GitHub profile pictures; the CSP allows images from it.
const GitHubAvatarOrigin = "https://avatars.githubusercontent.com"

// WantsJSON reports whether an in-place form save asked for a JSON result
// instead of the redirect a plain HTML form receives.
func WantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// WriteJSON sends a small, uncached JSON result for an in-place form save.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// FormResult is the JSON shape in-place form saves receive.
type FormResult struct {
	Message  string            `json:"message,omitempty"`
	Error    string            `json:"error,omitempty"`
	Revision int64             `json:"revision,omitempty"`
	Fields   map[string]string `json:"fields,omitempty"`
}
