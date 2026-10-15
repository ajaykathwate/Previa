// Package resourceid formats public resource addresses from handler-supplied display data.
package resourceid

import (
	"context"
	"fmt"
	"github.com/a-h/templ"
	"net/url"
	"strconv"
	"strings"
)

// ResourceRef describes a resource already authorized and selected by a handler.
// Pull-request references use the GitHub number as ID and the local repository as Parent.
type ResourceRef struct {
	Kind       string
	ID, Parent int64
}
type PublicIDs map[ResourceRef]string
type publicIDsKey struct{}

func WithPublicIDs(ctx context.Context, ids PublicIDs) context.Context {
	return context.WithValue(ctx, publicIDsKey{}, ids)
}
func PublicIDsFrom(ctx context.Context) (PublicIDs, bool) {
	v, ok := ctx.Value(publicIDsKey{}).(PublicIDs)
	return v, ok
}
func ResourceRefs(kind string, ids ...int64) []ResourceRef {
	out := make([]ResourceRef, 0, len(ids))
	for _, id := range ids {
		if id > 0 {
			out = append(out, ResourceRef{Kind: kind, ID: id})
		}
	}
	return out
}
func PRRefs(repo int64, number int32) []ResourceRef {
	return []ResourceRef{{Kind: "repository", ID: repo}, {Kind: "pull-request", ID: int64(number), Parent: repo}}
}
func (ids PublicIDs) Value(kind string, id, parent int64) (string, error) {
	if id <= 0 {
		return "", fmt.Errorf("missing %s reference", kind)
	}
	if value := ids[ResourceRef{Kind: kind, ID: id, Parent: parent}]; value != "" {
		return value, nil
	}
	return "", fmt.Errorf("public %s identifier was not supplied for the view", kind)
}
func PublicValue(ctx context.Context, kind string, id int64) (string, error) {
	ids, strict := PublicIDsFrom(ctx)
	if !strict {
		return strconv.FormatInt(id, 10), nil
	}
	return ids.Value(kind, id, 0)
}

// ResourceToken is for DOM identifiers. Missing references still fail in PublicURL
// and form-value attributes, instead of exposing an internal key.
func ResourceToken(ctx context.Context, kind string, id int64) string {
	ids, strict := PublicIDsFrom(ctx)
	if !strict {
		return strconv.FormatInt(id, 10)
	}
	return ids[ResourceRef{Kind: kind, ID: id}]
}
func publicSegment(ids PublicIDs, kind, value string, parent int64) (string, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return value, nil
	}
	return ids.Value(kind, id, parent)
}
func (ids PublicIDs) Internal(kind, value string) int64 {
	if id, err := strconv.ParseInt(value, 10, 64); err == nil {
		return id
	}
	for ref, public := range ids {
		if ref.Kind == kind && public == value {
			return ref.ID
		}
	}
	return 0
}

// PublicURL formats application-owned resource addresses using display data only.
// GitHub PR numbers remain labels, while Previa PR paths use their UUIDs.
func PublicURL[T ~string](ctx context.Context, raw T) (templ.SafeURL, error) {
	ids, strict := PublicIDsFrom(ctx)
	if !strict {
		return templ.URL(string(raw)), nil
	}
	value, err := ids.URL(string(raw))
	return templ.URL(value), err
}
func (ids PublicIDs) URL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse resource URL: %w", err)
	}
	if u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/") {
		return raw, nil
	}
	parts := strings.Split(u.Path, "/")
	if err := ids.path(parts); err != nil {
		return "", err
	}
	u.Path = strings.Join(parts, "/")
	q := u.Query()
	if value := q.Get("pr"); value != "" && len(parts) > 2 && parts[1] == "repositories" {
		value, err = publicSegment(ids, "pull-request", value, ids.Internal("repository", parts[2]))
		if err != nil {
			return "", err
		}
		q.Set("pr", value)
	}

	if v := q.Get("repo_id"); v != "" {
		v, err = publicSegment(ids, "repository", v, 0)
		if err != nil {
			return "", err
		}
		q.Set("repo_id", v)
	}
	if err := ids.dismissed(q); err != nil {
		return "", err
	}
	if u.RawQuery != "" {
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}
func (ids PublicIDs) path(parts []string) error {
	if len(parts) < 3 {
		return nil
	}
	var err error
	switch parts[1] {
	case "avatars":
		parts[2], err = publicSegment(ids, "user", parts[2], 0)
	case "repositories":
		repo := ids.Internal("repository", parts[2])
		parts[2], err = publicSegment(ids, "repository", parts[2], 0)
		if err != nil {
			return err
		}
		if len(parts) > 4 {
			switch parts[3] {
			case "pull-requests":
				parts[4], err = publicSegment(ids, "pull-request", parts[4], repo)
			case "outbound":
				parts[4], err = publicSegment(ids, "destination", parts[4], 0)
			}
		}
	case "workspaces":
		parts[2], err = publicSegment(ids, "workspace", parts[2], 0)
		if err != nil {
			return err
		}
		if len(parts) > 4 {
			switch parts[3] {
			case "members":
				parts[4], err = publicSegment(ids, "user", parts[4], 0)
			case "invitations":
				parts[4], err = publicSegment(ids, "invitation", parts[4], 0)
			}
		}
	}
	return err
}

// PublicQuery formats selected pull-request state carried between workspace forms.
func PublicQuery(ctx context.Context, repo int64, raw string) (string, error) {
	ids, strict := PublicIDsFrom(ctx)
	if !strict {
		return raw, nil
	}
	value, err := ids.URL("/repositories/" + strconv.FormatInt(repo, 10) + "?" + raw)
	if err != nil {
		return "", err
	}
	_, query, _ := strings.Cut(value, "?")
	return query, nil
}

func (ids PublicIDs) dismissed(q url.Values) error {
	value := q.Get("dismissed")
	repo, number, ok := strings.Cut(value, "-")
	if !ok || strings.Contains(value, ".") {
		return nil
	}
	id, err := strconv.ParseInt(repo, 10, 64)
	if err != nil {
		return nil
	}
	repo, err = publicSegment(ids, "repository", repo, 0)
	if err != nil {
		return err
	}
	number, err = publicSegment(ids, "pull-request", number, id)
	if err != nil {
		return err
	}
	q.Set("dismissed", repo+"."+number)
	return nil
}
