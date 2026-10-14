// Package scope carries verified request selection, never authorization evidence.
package scope

import "context"

type key struct{}
type Selection struct {
	WorkspaceID int64
	Role        string
}

func With(ctx context.Context, id int64, role string) context.Context {
	return context.WithValue(ctx, key{}, Selection{WorkspaceID: id, Role: role})
}
func Current(ctx context.Context) Selection {
	s, _ := ctx.Value(key{}).(Selection)
	return s
}
func WorkspaceID(ctx context.Context) int64 { return Current(ctx).WorkspaceID }
