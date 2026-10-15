package publicid

import (
	"context"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type catalogKey struct{}

func (c *Catalog) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), catalogKey{}, c)))
	})
}
func from(ctx context.Context) *Catalog { c, _ := ctx.Value(catalogKey{}).(*Catalog); return c }
func Enabled(ctx context.Context) bool  { return from(ctx) != nil }

func Value(r *http.Request, kind, value string, parent int64) (int64, error) {
	c := from(r.Context())
	if c == nil {
		return parseLegacy(value)
	}
	// Existing HTML bookmarks are authorized normally, then redirected by Handler.
	// Numeric mutation, stream and download addresses are never accepted.
	if r.Method == http.MethodGet && htmlPath(r.URL.Path) {
		if id, err := parseLegacy(value); err == nil {
			return id, nil
		}
	}
	return c.Resolve(r.Context(), kind, value, parent)
}
func Param(r *http.Request, name, kind string) (int64, error) {
	return Value(r, kind, chi.URLParam(r, name), 0)
}
func PRNumber(r *http.Request, repo int64) (int64, error) {
	return Value(r, "pull-request", chi.URLParam(r, "number"), repo)
}
func htmlPath(path string) bool {
	for _, suffix := range []string{"/events", "/recent", "/download", "/logo", "/build", "/teardown", "/remove"} {
		if strings.HasSuffix(path, suffix) {
			return false
		}
	}
	return strings.HasPrefix(path, "/repositories/") || strings.HasPrefix(path, "/workspaces/") || path == "/builds" || path == "/previews"
}

// Public turns an internal ID into its public UUID for a redirect or callback.
func Public(ctx context.Context, kind string, id, parent int64) (string, error) {
	if c := from(ctx); c != nil {
		return c.Public(ctx, kind, id, parent)
	}
	return strconv.FormatInt(id, 10), nil
}

func PathID(r *http.Request, name, kind string) int64 {
	id, err := Param(r, name, kind)
	if err != nil {
		return 0
	}
	return id
}
func FormID(r *http.Request, name, kind string) int64 {
	id, err := Value(r, kind, r.PostFormValue(name), 0)
	if err != nil {
		return 0
	}
	return id
}

func Dismissed(r *http.Request, value string, legacy func(string) (int64, int32)) (int64, int32) {
	if c := from(r.Context()); c != nil {
		repo, pr, ok := strings.Cut(value, ".")
		if !ok {
			return 0, 0
		}
		id, err := c.Resolve(r.Context(), "repository", repo, 0)
		if err != nil {
			return 0, 0
		}
		number, err := c.Resolve(r.Context(), "pull-request", pr, id)
		if err != nil {
			return 0, 0
		}
		return id, int32(number)
	}
	return legacy(value)
}

// Selection normalizes a public pull-request selector before feature filter parsing.
func Selection(ctx context.Context, repo int64, params url.Values) error {
	c := from(ctx)
	value := params.Get("pr")
	if c == nil || value == "" {
		return nil
	}
	if _, err := parseLegacy(value); err == nil {
		return nil
	}
	number, err := c.Resolve(ctx, "pull-request", value, repo)
	if err != nil {
		return err
	}
	params.Set("pr", strconv.FormatInt(number, 10))
	return nil
}
