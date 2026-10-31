package workspaces

import (
	"bytes"
	"encoding/json"
	"image"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ajaykathwatee/previa/internal/platform/publicid"
)

// send posts a form as an in-place save (Accept: application/json) or as a
// plain HTML form; a non-nil file makes it multipart.
func (b *testBrowser) send(t *testing.T, path string, values url.Values, field string, file []byte, asJSON bool) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	contentType := "application/x-www-form-urlencoded"
	if file != nil {
		form := multipart.NewWriter(&body)
		for key := range values {
			if err := form.WriteField(key, values.Get(key)); err != nil {
				t.Fatal(err)
			}
		}
		part, err := form.CreateFormFile(field, "upload.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(file); err != nil {
			t.Fatal(err)
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		contentType = form.FormDataContentType()
	} else {
		body.WriteString(values.Encode())
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost"+path, &body)
	req.Header.Set("Content-Type", contentType)
	if asJSON {
		req.Header.Set("Accept", "application/json")
	}
	for _, c := range b.cookies {
		req.AddCookie(c)
	}
	res := httptest.NewRecorder()
	b.router.ServeHTTP(res, req)
	return res
}

type formResult struct {
	Message, Error string
	Revision       int64
	Fields         map[string]string
}

func decodeResult(t *testing.T, r *httptest.ResponseRecorder, status int) formResult {
	t.Helper()
	var v formResult
	if r.Code != status || r.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("want %d JSON, got %d %s: %s", status, r.Code, r.Header().Get("Content-Type"), r.Body)
	}
	if err := json.Unmarshal(r.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestProfilePhotosReachTeammatesOnlyAndSavesAnswerInPlace(t *testing.T) {
	f := newFixture(t)
	f.invite(t, "admin", "admin", f.admin, 102, nil)
	router := configuredFixtureRouter(t, f, "http://localhost", true)
	owner := testBrowser{router: router, cookies: map[string]*http.Cookie{}}
	owner.login(t, "owner")
	csrf := owner.csrf(t)

	if r := owner.send(t, "/protected/profile/avatar", url.Values{}, "avatar", logoPNG(t, 8, 8), true); r.Code != http.StatusForbidden {
		t.Fatalf("photo upload without CSRF: %d", r.Code)
	}
	saved := decodeResult(t, owner.send(t, "/protected/profile/avatar", url.Values{"csrf_token": {csrf}}, "avatar", logoPNG(t, 600, 400), true), http.StatusOK)
	src := saved.Fields["avatar"]
	if !regexp.MustCompile(`^/avatars/[0-9a-f-]{36}\?v=1$`).MatchString(src) || saved.Message != "Profile photo updated" {
		t.Fatalf("upload result: %+v", saved)
	}
	path, _, _ := strings.Cut(src, "?")
	photo := owner.request(http.MethodGet, path, nil)
	img, format, err := image.Decode(bytes.NewReader(photo.Body.Bytes()))
	if photo.Code != 200 || err != nil || format != "png" || img.Bounds().Dx() != 256 || img.Bounds().Dy() != 256 {
		t.Fatalf("own photo: %d %v %s", photo.Code, err, format)
	}
	etag := photo.Header().Get("ETag")
	req := httptest.NewRequest(http.MethodGet, "http://localhost"+path, nil)
	req.Header.Set("If-None-Match", etag)
	for _, c := range owner.cookies {
		req.AddCookie(c)
	}
	cached := httptest.NewRecorder()
	router.ServeHTTP(cached, req)
	if cached.Code != http.StatusNotModified {
		t.Fatalf("revalidated photo: %d", cached.Code)
	}
	members := owner.request(http.MethodGet, "/workspaces/"+publicWorkspace(t, f)+"/members", nil)
	if !strings.Contains(members.Body.String(), `src="`+src+`"`) {
		t.Fatal("members list did not show the uploaded photo")
	}

	for login, want := range map[string]int{"admin": http.StatusOK, "outsider": http.StatusNotFound} {
		b := testBrowser{router: router, cookies: map[string]*http.Cookie{}}
		b.login(t, login)
		if r := b.request(http.MethodGet, path, nil); r.Code != want {
			t.Fatalf("%s photo access: %d, want %d", login, r.Code, want)
		}
	}
	numeric := strings.Replace(path, path[len("/avatars/"):], strconv.FormatInt(f.owner, 10), 1)
	if r := owner.request(http.MethodGet, numeric, nil); r.Code != http.StatusNotFound {
		t.Fatalf("numeric photo address accepted: %d", r.Code)
	}

	removed := decodeResult(t, owner.send(t, "/protected/profile/avatar/remove", url.Values{"csrf_token": {csrf}}, "", nil, true), http.StatusOK)
	if removed.Fields["avatar"] != "/static/brand/favicon.svg" {
		t.Fatalf("removal should fall back to the GitHub picture: %+v", removed)
	}
	if r := owner.request(http.MethodGet, path, nil); r.Code != http.StatusNotFound {
		t.Fatalf("removed photo still served: %d", r.Code)
	}
	if r := owner.send(t, "/protected/profile/avatar", url.Values{"csrf_token": {csrf}}, "avatar", []byte("<svg/>"), true); decodeResult(t, r, http.StatusBadRequest).Error == "" {
		t.Fatal("invalid photo lacked an inline error")
	}

	name := decodeResult(t, owner.send(t, "/protected/profile", url.Values{"csrf_token": {csrf}, "profile_revision": {"1"}, "first_name": {" Ada "}, "last_name": {"Lovelace"}}, "", nil, true), http.StatusOK)
	if name.Revision != 2 || name.Fields["first_name"] != "Ada" || name.Fields["display_name"] != "Ada Lovelace" {
		t.Fatalf("in-place name save: %+v", name)
	}
	if stale := decodeResult(t, owner.send(t, "/protected/profile", url.Values{"csrf_token": {csrf}, "profile_revision": {"1"}, "first_name": {"Stale"}}, "", nil, true), http.StatusConflict); !strings.Contains(stale.Error, "another window") {
		t.Fatalf("stale name: %+v", stale)
	}

	w, err := f.service.Read(t.Context(), f.owner, f.wid)
	if err != nil {
		t.Fatal(err)
	}
	settings := "/workspaces/" + publicWorkspace(t, f) + "/settings"
	details := decodeResult(t, owner.send(t, settings, url.Values{"csrf_token": {csrf}, "profile_revision": {strconv.FormatInt(w.ProfileRevision, 10)}, "name": {" Orion "}, "description": {"Previews"}}, "", nil, true), http.StatusOK)
	if details.Revision != w.ProfileRevision+1 || details.Fields["name"] != "Orion" {
		t.Fatalf("in-place workspace save: %+v", details)
	}
	if stale := decodeResult(t, owner.send(t, settings, url.Values{"csrf_token": {csrf}, "profile_revision": {strconv.FormatInt(w.ProfileRevision, 10)}, "name": {"Stale"}}, "", nil, true), http.StatusConflict); stale.Error == "" {
		t.Fatal("stale workspace save lacked an error")
	}
	if plain := owner.send(t, settings, url.Values{"csrf_token": {csrf}, "profile_revision": {strconv.FormatInt(details.Revision, 10)}, "name": {"Orion"}, "description": {""}}, "", nil, false); plain.Code != http.StatusSeeOther || !strings.Contains(plain.Header().Get("Location"), "saved=details") {
		t.Fatalf("plain form save: %d %s", plain.Code, plain.Header().Get("Location"))
	}
}

func publicWorkspace(t *testing.T, f fixture) string {
	t.Helper()
	id, err := publicid.New(f.pool).Public(t.Context(), "workspace", f.wid, 0)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
