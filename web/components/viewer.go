package components

import "context"

type Viewer struct{ Name, Login, Avatar, PublicID string }
type viewerKey struct{}

func WithViewer(ctx context.Context, v Viewer) context.Context {
	return context.WithValue(ctx, viewerKey{}, v)
}
func CurrentViewer(ctx context.Context) Viewer { v, _ := ctx.Value(viewerKey{}).(Viewer); return v }
