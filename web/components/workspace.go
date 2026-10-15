package components

import (
	"context"
	"strconv"
)

// WorkspaceShell is display data supplied by the authenticated handler.
type WorkspaceShell struct {
	ID               int64
	LogoVersion      int64
	Invitations      int64 // pending invitations addressed to the signed-in person
	Name, Role, CSRF string
	Items            []WorkspaceItem
}
type WorkspaceItem struct {
	ID          int64
	LogoVersion int64
	Name, Role  string
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

func WorkspaceURL(id int64, section string) string {
	return "/workspaces/" + strconv.FormatInt(id, 10) + "/" + section
}
func WorkspaceLogoURL(id, version int64) string {
	return WorkspaceURL(id, "logo") + "?v=" + strconv.FormatInt(version, 10)
}
func RoleName(role string) string {
	switch role {
	case "owner":
		return "Owner"
	case "admin":
		return "Admin"
	case "developer":
		return "Developer"
	case "reviewer":
		return "Reviewer"
	default:
		return "Member"
	}
}
