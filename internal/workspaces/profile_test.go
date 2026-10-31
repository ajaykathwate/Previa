package workspaces

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

func seedPreview(t *testing.T, f fixture) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO environments(pull_request_id,subdomain,status,container_id,image_tag,head_sha) VALUES($1,'team-preview','running','fixture-container','fixture-image','old')`, f.pr); err != nil {
		t.Fatal(err)
	}
}
func logoPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var b bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.NRGBA{R: 130, G: 190, B: 30, A: 255})
		}
	}
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestNormalizeWorkspaceLogo(t *testing.T) {
	pngData := logoPNG(t, 8, 8)
	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{pngData, jpegData.Bytes(), append(append([]byte{}, pngData...), []byte("trailing metadata")...)} {
		got, err := normalizeLogo(data)
		if err != nil {
			t.Fatal(err)
		}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(got))
		if err != nil || format != "png" || cfg.Width != 8 {
			t.Fatalf("normalized logo: %+v %s %v", cfg, format, err)
		}
		if bytes.Contains(got, []byte("metadata")) {
			t.Fatal("retained trailing metadata")
		}
	}
	for _, data := range [][]byte{nil, []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), pngData[:25], make([]byte, MaxLogoBytes+1), logoPNG(t, 513, 1)} {
		if _, err := normalizeLogo(data); !errors.Is(err, ErrLogo) {
			t.Fatalf("invalid image accepted: %v", err)
		}
	}
}
func TestWorkspaceProfilesUseIndependentRevisionAndLocalAuthority(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.invite(t, "admin", "admin", f.admin, 102, nil)
	f.invite(t, "dev", "developer", f.dev, 103, []int64{f.repo})
	f.invite(t, "reviewer", "reviewer", f.reviewer, 104, []int64{f.repo})
	_ = pendingPublication(t, f, "running")
	w, err := f.service.Read(ctx, f.owner, f.wid)
	if err != nil {
		t.Fatal(err)
	}
	f.github.fail = true
	if err := f.service.UpdateProfile(ctx, f.admin, f.wid, w.ProfileRevision, " Team Orion ", " API previews "); err != nil {
		t.Fatal(err)
	}
	after, err := f.service.Read(ctx, f.owner, f.wid)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "Team Orion" || after.Description != "API previews" || after.ProfileRevision != w.ProfileRevision+1 || after.Revision != w.Revision {
		t.Fatalf("profile revision leaked into access: %+v", after)
	}
	var state string
	if err := f.pool.QueryRow(ctx, "SELECT state FROM build_intents WHERE pull_request_id=$1", f.pr).Scan(&state); err != nil || state != "running" {
		t.Fatalf("profile edit interrupted request: %s %v", state, err)
	}
	if err := f.service.UpdateProfile(ctx, f.owner, f.wid, w.ProfileRevision, "Stale", "lost update"); !errors.Is(err, ErrConflict) {
		t.Fatal("stale profile accepted", err)
	}
	for _, user := range []int64{f.dev, f.reviewer, 9999} {
		if err := f.service.UpdateProfile(ctx, user, f.wid, after.ProfileRevision, "Forbidden", ""); !errors.Is(err, ErrUnavailable) {
			t.Fatal("unauthorized profile edit", user, err)
		}
		if err := f.service.SetLogo(ctx, user, f.wid, after.ProfileRevision, logoPNG(t, 8, 8)); !errors.Is(err, ErrUnavailable) {
			t.Fatal("unauthorized logo edit", user, err)
		}
	}
	for _, input := range [][2]string{{"", ""}, {strings.Repeat("a", 81), ""}, {"name\nline", ""}, {"name", strings.Repeat("語", 241)}, {"name", "nul\x00"}} {
		if err := f.service.UpdateProfile(ctx, f.owner, f.wid, after.ProfileRevision, input[0], input[1]); !errors.Is(err, ErrInput) {
			t.Fatal("invalid profile accepted", err)
		}
	}
	if err := f.service.SetLogo(ctx, f.owner, f.wid, after.ProfileRevision, logoPNG(t, 8, 8)); err != nil {
		t.Fatal(err)
	}
	data, version, err := f.service.Logo(ctx, f.reviewer, f.wid)
	if err != nil || len(data) == 0 || version != after.ProfileRevision+1 {
		t.Fatal("member logo read", version, err)
	}
	if _, _, err := f.service.Logo(ctx, 9999, f.wid); !errors.Is(err, ErrUnavailable) {
		t.Fatal("outsider logo exposed", err)
	}
	updated, err := f.service.Read(ctx, f.owner, f.wid)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.RemoveLogo(ctx, f.admin, f.wid, updated.ProfileRevision); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.service.Logo(ctx, f.owner, f.wid); !errors.Is(err, ErrUnavailable) {
		t.Fatal("logo not removed", err)
	}
	final, err := f.service.Read(ctx, f.owner, f.wid)
	if err != nil || final.LogoVersion != 0 || final.Revision != w.Revision {
		t.Fatal("logo removed access revision", final, err)
	}
}
func (b *testBrowser) upload(t *testing.T, path, token, revision string, data []byte, origin string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for key, val := range map[string]string{"csrf_token": token, "profile_revision": revision} {
		if err := form.WriteField(key, val); err != nil {
			t.Fatal(err)
		}
	}
	part, err := form.CreateFormFile("logo", "team.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost"+path, &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for _, c := range b.cookies {
		req.AddCookie(c)
	}
	res := httptest.NewRecorder()
	b.router.ServeHTTP(res, req)
	for _, c := range res.Result().Cookies() {
		b.cookies[c.Name] = c
	}
	return res
}
func TestWorkspaceProfileFormsAndMultipartCSRF(t *testing.T) {
	f := newFixture(t)
	b := testBrowser{router: fixtureRouter(t, f), cookies: map[string]*http.Cookie{}}
	b.login(t, "owner")
	token := b.csrf(t)
	base := "/workspaces/" + url.PathEscape(strconv.FormatInt(f.wid, 10))
	w, err := f.service.Read(t.Context(), f.owner, f.wid)
	if err != nil {
		t.Fatal(err)
	}
	revision := strconv.FormatInt(w.ProfileRevision, 10)
	for _, tt := range []struct{ token, origin string }{{"", ""}, {token, "https://attacker.test"}} {
		r := b.upload(t, base+"/logo", tt.token, revision, logoPNG(t, 8, 8), tt.origin)
		if r.Code != 403 {
			t.Fatal("multipart CSRF bypass", r.Code)
		}
	}
	r := b.upload(t, base+"/logo", token, revision, logoPNG(t, 8, 8), "http://localhost")
	if r.Code != 303 {
		t.Fatalf("multipart upload: %d %s", r.Code, r.Body.String())
	}
	r = b.request(http.MethodGet, base+"/logo", nil)
	if r.Code != 200 || r.Header().Get("Content-Type") != "image/png" || r.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("private logo response", r.Code, r.Header())
	}
	r = b.upload(t, base+"/logo", token, revision, logoPNG(t, 8, 8), "")
	if r.Code != 409 || !strings.Contains(r.Body.String(), "another window") {
		t.Fatal("stale upload accepted", r.Code)
	}
	w, err = f.service.Read(t.Context(), f.owner, f.wid)
	if err != nil {
		t.Fatal(err)
	}
	r = b.upload(t, base+"/logo", token, strconv.FormatInt(w.ProfileRevision, 10), []byte("svg"), "")
	if r.Code != 400 || !strings.Contains(r.Body.String(), "PNG or JPEG logo") || strings.Count(r.Body.String(), "PNG or JPEG logo") != 1 {
		t.Fatal("invalid upload lost page context", r.Code)
	}
	r = b.request(http.MethodPost, base+"/settings", url.Values{"csrf_token": {token}, "profile_revision": {strconv.FormatInt(w.ProfileRevision, 10)}, "name": {""}, "description": {"Keep my description"}})
	if r.Code != 400 || !strings.Contains(r.Body.String(), "Keep my description") {
		t.Fatal("form input lost", r.Code)
	}
	for _, login := range []string{"dev", "reviewer"} {
		f.invite(t, login, map[string]string{"dev": "developer", "reviewer": "reviewer"}[login], map[string]int64{"dev": f.dev, "reviewer": f.reviewer}[login], map[string]int64{"dev": 103, "reviewer": 104}[login], []int64{f.repo})
		b.login(t, login)
		r = b.request(http.MethodGet, base+"/settings", nil)
		if r.Code != 404 {
			t.Fatal("non-admin settings exposed", login, r.Code)
		}
	}
}

func TestWorkspacePeopleSearchKeepsPaginationAndInvitationScope(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	for i := range 25 {
		var id int64
		if err := f.pool.QueryRow(ctx, "INSERT INTO users(github_id,username) VALUES($1,$2) RETURNING id", 1000+i, fmt.Sprintf("member-%02d", i)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, "INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,'developer')", f.wid, id); err != nil {
			t.Fatal(err)
		}
	}
	p, err := f.service.Page(ctx, f.owner, f.wid, "members", PageQuery{Page: 2})
	if err != nil || p.PageCount != 2 || len(p.Members) != 6 {
		t.Fatalf("member pagination %+v %v", p, err)
	}
	p, err = f.service.Page(ctx, f.owner, f.wid, "members", PageQuery{Page: 99, Search: "MeMbEr-24"})
	if err != nil || p.PageCount != 1 || p.PageNumber != 1 || len(p.Members) != 1 || p.Members[0].Login != "member-24" {
		t.Fatalf("search across pages %+v %v", p, err)
	}
	p, err = f.service.Page(ctx, f.owner, f.wid, "members", PageQuery{Search: "%"})
	if err != nil || len(p.Members) != 0 {
		t.Fatal("search treated input as wildcard", err)
	}
	w, err := f.service.Read(ctx, f.owner, f.wid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Invite(ctx, InvitationCommand{ActorID: f.owner, WorkspaceID: f.wid, Revision: w.Revision, Login: "newbie", Role: "reviewer", Repositories: []int64{f.repo}}); err != nil {
		t.Fatal(err)
	}
	p, err = f.service.Page(ctx, f.owner, f.wid, "members", PageQuery{Invitations: true, Search: "NEWBIE"})
	if err != nil || len(p.Invitations) != 1 || len(p.Repositories) != 2 || len(p.Members) != 0 {
		t.Fatalf("invitation list lost scopes %+v %v", p, err)
	}
	if !strings.Contains(p.PageURL(2), "q=NEWBIE") || !strings.Contains(p.PageURL(2), "view=invitations") {
		t.Fatal("lost filter in pagination", p.PageURL(2))
	}
}
func TestWorkspaceProfileMigrationPreservesExistingTeams(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	b, err := os.ReadFile("../../db/migrations/0031_workspace_profiles.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(b), "-- +goose Down")
	if !ok {
		t.Fatal("missing migration down")
	}
	if _, err := f.pool.Exec(ctx, down); err != nil {
		t.Fatal("profile rollback", err)
	}
	var owner int64
	if err := f.pool.QueryRow(ctx, "SELECT owner_user_id FROM workspaces WHERE id=$1", f.wid).Scan(&owner); err != nil || owner != f.owner {
		t.Fatal("profile rollback lost team", err)
	}
	if _, err := f.pool.Exec(ctx, up); err != nil {
		t.Fatal("profile migrate", err)
	}
	w, err := f.service.Read(ctx, f.owner, f.wid)
	if err != nil || w.ProfileRevision != 1 || w.Description != "" || w.LogoVersion != 0 {
		t.Fatal("profile defaults", w, err)
	}
}
