package resourceid

import (
	"context"
	"net/url"
	"testing"
)

func TestPublicResourceURLs(t *testing.T) {
	ids := PublicIDs{{Kind: "repository", ID: 17}: "80736c1e-e2fa-49b7-95fb-12345c53cd74", {Kind: "pull-request", ID: 4, Parent: 17}: "15986a73-4b1c-4cc6-8796-0323c6e96787", {Kind: "workspace", ID: 3}: "fd792d70-bd87-42e2-a26d-1a5f98df437e", {Kind: "user", ID: 9}: "b869a1a2-ff1d-44d3-94af-f71f8dc20c31"}
	repo, _ := ids.Value("repository", 17, 0)
	pr, _ := ids.Value("pull-request", 4, 17)
	workspace, _ := ids.Value("workspace", 3, 0)
	member, _ := ids.Value("user", 9, 0)
	for _, tc := range []struct{ in, want string }{
		{"/repositories/17/pull-requests/4/logs/events", "/repositories/" + repo + "/pull-requests/" + pr + "/logs/events"},
		{"/workspaces/3/members/9", "/workspaces/" + workspace + "/members/" + member},
		{"/builds?repo_id=17&dismissed=17-4", "/builds?dismissed=" + repo + "." + pr + "&repo_id=" + repo},
		{"https://github.com/acme/app/pull/4", "https://github.com/acme/app/pull/4"},
		{"/repositories/" + repo + "/pull-requests/" + pr, "/repositories/" + repo + "/pull-requests/" + pr},
	} {
		got, err := ids.URL(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%s -> %s: %v; want %s", tc.in, got, err, tc.want)
		}
	}
	if _, err := ids.URL("/repositories/18"); err == nil {
		t.Fatal("missing view reference exposed a numeric key")
	}
	query, err := PublicQuery(WithPublicIDs(context.Background(), ids), 17, "pr=4&pr_q=hello")
	values, _ := url.ParseQuery(query)
	if err != nil || values.Get("pr") != pr || values.Get("pr_q") != "hello" {
		t.Fatalf("query state: %s %v", query, err)
	}
	value, err := PublicURL(WithPublicIDs(context.Background(), ids), "javascript:alert(1)")
	if err != nil || value != "about:invalid#TemplFailedSanitizationURL" {
		t.Fatalf("unsafe URL: %s %v", value, err)
	}
}
