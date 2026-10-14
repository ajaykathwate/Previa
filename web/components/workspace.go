package components

import "context"

// WorkspaceShell is display data supplied by the authenticated handler.
type WorkspaceShell struct {
	ID               int64
	Name, Role, CSRF string
	Items            []WorkspaceItem
}
type WorkspaceItem struct {
	ID         int64
	Name, Role string
}
type workspaceKey struct{}

func WithWorkspace(ctx context.Context, shell WorkspaceShell) context.Context {
	return context.WithValue(ctx, workspaceKey{}, shell)
}
func Workspace(ctx context.Context) WorkspaceShell {
	s, _ := ctx.Value(workspaceKey{}).(WorkspaceShell)
	return s
}
func CanOperate(ctx context.Context) bool {
	w, present := ctx.Value(workspaceKey{}).(WorkspaceShell)
	if !present {
		return true
	}
	return w.ID > 0 && w.Role != "reviewer"
}
func CanManage(ctx context.Context) bool {
	w, present := ctx.Value(workspaceKey{}).(WorkspaceShell)
	if !present {
		return true
	}
	return w.ID > 0 && (w.Role == "owner" || w.Role == "admin")
}
func WorkspaceInitial(name string) string {
	for _, r := range name {
		return string(r)
	}
	return "P"
}
