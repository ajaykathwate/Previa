package publicid

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/ajaykathwatee/previa/web/components"
)

type references interface {
	PublicReferences() []components.ResourceRef
}

// Handler batches the IDs present in display data before rendering. Templates
// format addresses from that data; they never query the database.
func Handler(r *http.Request, component templ.Component, data any, options ...func(*templ.ComponentHandler)) http.Handler {
	if from(r.Context()) == nil {
		return templ.Handler(component, options...)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		refs := shellRefs(r.Context())
		if view, ok := data.(references); ok {
			refs = append(refs, view.PublicReferences()...)
		}
		if extra, ok := data.([]components.ResourceRef); ok {
			refs = append(refs, extra...)
		}
		ids, err := from(r.Context()).Load(r.Context(), refs)
		if err != nil {
			renderError(w, r, err)
			return
		}
		if _, ok := data.(references); ok && r.Method == http.MethodGet && len(urlRefs(r.URL.RequestURI())) > 0 && r.Header.Get("HX-Request") == "" {
			canonical, e := ids.URL(r.URL.RequestURI())
			if e != nil {
				renderError(w, r, e)
				return
			}
			if canonical != r.URL.RequestURI() {
				http.Redirect(w, r, canonical, http.StatusSeeOther)
				return
			}
		}
		next := r.WithContext(components.WithPublicIDs(r.Context(), ids))
		templ.Handler(component, options...).ServeHTTP(w, next)
	})
}
func shellRefs(ctx context.Context) []components.ResourceRef {
	shell := components.Workspace(ctx)
	refs := components.ResourceRefs("workspace", shell.ID)
	for _, item := range shell.Items {
		refs = append(refs, components.ResourceRef{Kind: "workspace", ID: item.ID})
	}
	return refs
}
func renderError(w http.ResponseWriter, r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "prepare public resource addresses", "error", err)
	http.Error(w, "This page could not be loaded. Please try again.", http.StatusServiceUnavailable)
}

func URL(r *http.Request, raw string) (string, error) {
	c := from(r.Context())
	if c == nil {
		return raw, nil
	}
	refs := urlRefs(raw)
	if len(refs) == 0 {
		return raw, nil
	}
	ids, err := c.Load(r.Context(), refs)
	if err != nil {
		return "", err
	}
	return ids.URL(raw)
}
func Redirect(w http.ResponseWriter, r *http.Request, raw string, status int) {
	target, err := URL(r, raw)
	if err != nil {
		renderError(w, r, err)
		return
	}
	http.Redirect(w, r, target, status)
}
func SetHeader(w http.ResponseWriter, r *http.Request, name, raw string) error {
	value, err := URL(r, raw)
	if err != nil {
		return err
	}
	w.Header().Set(name, value)
	return nil
}
func urlRefs(raw string) []components.ResourceRef {
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" {
		return nil
	}
	p := strings.Split(u.Path, "/")
	var refs []components.ResourceRef
	add := func(kind, raw string, parent int64) {
		if id, e := strconv.ParseInt(raw, 10, 64); e == nil && id > 0 {
			refs = append(refs, components.ResourceRef{Kind: kind, ID: id, Parent: parent})
		}
	}
	if len(p) > 2 {
		switch p[1] {
		case "workspaces":
			add("workspace", p[2], 0)
			if len(p) > 4 {
				switch p[3] {
				case "members":
					add("user", p[4], 0)
				case "invitations":
					add("invitation", p[4], 0)
				}
			}
		case "repositories":
			add("repository", p[2], 0)
			if len(p) > 4 {
				switch p[3] {
				case "pull-requests":
					id, _ := strconv.ParseInt(p[2], 10, 64)
					add("pull-request", p[4], id)
				case "outbound":
					add("destination", p[4], 0)
				}
			}
		}
	}
	if len(p) > 2 && p[1] == "repositories" {
		repo, _ := strconv.ParseInt(p[2], 10, 64)
		add("pull-request", u.Query().Get("pr"), repo)
	}
	add("repository", u.Query().Get("repo_id"), 0)
	if repo, num, ok := strings.Cut(u.Query().Get("dismissed"), "-"); ok && !strings.Contains(repo, ".") {
		id, _ := strconv.ParseInt(repo, 10, 64)
		add("repository", repo, 0)
		add("pull-request", num, id)
	}
	return refs
}
