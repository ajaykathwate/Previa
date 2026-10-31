package components

import (
	"context"
	"github.com/a-h/templ"
	ids "github.com/ajaykathwatee/previa/web/components/resourceid"
)

type ResourceRef = ids.ResourceRef
type PublicIDs = ids.PublicIDs

func WithPublicIDs(ctx context.Context, v PublicIDs) context.Context {
	return ids.WithPublicIDs(ctx, v)
}
func PublicIDsFrom(ctx context.Context) (PublicIDs, bool) { return ids.PublicIDsFrom(ctx) }
func ResourceRefs(kind string, values ...int64) []ResourceRef {
	return ids.ResourceRefs(kind, values...)
}
func PRRefs(repo int64, number int32) []ResourceRef { return ids.PRRefs(repo, number) }
func PublicURL[T ~string](ctx context.Context, value T) (templ.SafeURL, error) {
	return ids.PublicURL(ctx, value)
}
func PublicValue(ctx context.Context, kind string, id int64) (string, error) {
	return ids.PublicValue(ctx, kind, id)
}
func ResourceToken(ctx context.Context, kind string, id int64) string {
	return ids.ResourceToken(ctx, kind, id)
}

func PublicQuery(ctx context.Context, repo int64, raw string) (string, error) {
	return ids.PublicQuery(ctx, repo, raw)
}
