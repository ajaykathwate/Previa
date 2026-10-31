package app

import "testing"

func TestWorkspaceSourcePermissionRequiresNumericGitHubIdentity(t *testing.T) {
	app := newFakeGitHub(t)
	access := workspaceGitHub{app: app}
	role, err := access.RepositoryUserPermission(t.Context(), 99, "octo/app", "owner", 1)
	if err != nil || role != "admin" {
		t.Fatal(role, err)
	}
	role, err = access.RepositoryUserPermission(t.Context(), 99, "octo/app", "owner", 999)
	if err != nil || role != "none" {
		t.Fatal("reclaimed username authorized", role, err)
	}
	if _, err := (workspaceGitHub{}).RepositoryUserPermission(t.Context(), 99, "octo/app", "owner", 1); err == nil {
		t.Fatal("missing App authorized")
	}
}
