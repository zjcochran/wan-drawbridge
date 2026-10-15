// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// testPNG returns a small PNG in one colour.
func testPNG(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := range 4 {
		for y := range 4 {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// uploadPicture posts a picture the way the Your account form does.
func (c *client) uploadPicture(data []byte) *http.Response {
	c.e.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if data != nil {
		fw, _ := mw.CreateFormFile("picture", "me.png")
		fw.Write(data)
	}
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, c.e.srv.URL+"/settings/picture", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", c.csrf)
	req.Header.Set("HX-Request", "true")
	resp, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func TestUploadAndServePicture(t *testing.T) {
	e := newEnv(t)
	admin := e.admin()
	me := e.userID("admin")

	if _, page := admin.get("/settings/account"); !strings.Contains(page, `<span class="avatar" aria-hidden="true">A</span>`) {
		t.Fatal("no initial without a picture")
	}
	pic := testPNG(t, color.RGBA{200, 80, 40, 255})
	if resp := admin.uploadPicture(pic); resp.StatusCode != http.StatusNoContent || !strings.HasPrefix(resp.Header.Get("HX-Redirect"), "/settings/account?flash=") {
		t.Fatalf("upload: %d %s", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
	u, _ := e.st.UserByID(context.Background(), me)
	src := "/users/" + strconv.Itoa(me) + "/picture?v=" + strconv.FormatInt(u.Picture, 10)
	_, page := admin.get("/settings/account")
	if !strings.Contains(page, `src="`+src+`"`) || !strings.Contains(page, "Remove picture") {
		t.Fatal("the account page does not show the uploaded picture")
	}
	resp, body := admin.get(src)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" || body != string(pic) {
		t.Fatalf("serve: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// Others see it only as admins.
	v := e.viewerClient()
	if resp, _ := v.get("/users/" + strconv.Itoa(me) + "/picture"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a viewer fetched someone else's picture: %d", resp.StatusCode)
	}
	if resp, _ := v.get("/users/" + strconv.Itoa(e.userID("viewer")) + "/picture"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a missing picture: %d", resp.StatusCode)
	}
	if resp, _ := admin.get("/users/x/picture"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a bad id: %d", resp.StatusCode)
	}

	// Bad uploads.
	for name, data := range map[string][]byte{
		"none":   nil,
		"text":   []byte("not a picture"),
		"svg":    []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`),
		"toobig": append(append([]byte{}, pic...), make([]byte, MaxPictureBytes)...),
	} {
		if resp := admin.uploadPicture(data); resp.StatusCode != http.StatusUnprocessableEntity && resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("%s upload: %d", name, resp.StatusCode)
		}
	}

	if resp, _ := admin.htmx("/settings/picture/remove", url.Values{}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove: %d", resp.StatusCode)
	}
	if u, _ := e.st.UserByID(context.Background(), me); u.Picture != 0 {
		t.Error("the picture was not removed")
	}
}

// userID returns the ID of the user with this name.
func (e *env) userID(name string) int {
	e.t.Helper()
	users, err := e.st.Users(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, u := range users {
		if u.Username == name {
			return u.ID
		}
	}
	e.t.Fatalf("no user %q", name)
	return 0
}

func TestSSOPicture(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	idp := newIDP(t)
	configureSSO(t, e.admin(), idp, nil)
	idp.picture = testPNG(t, color.RGBA{40, 120, 200, 255})
	idp.as(map[string]any{"sub": "user-1", "preferred_username": "alice", "picture": idp.srv.URL + "/picture.png"})

	c := e.client()
	if resp, _ := c.ssoLogin("/auth/oidc/login"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sso sign-in: %d", resp.StatusCode)
	}
	u, _, _ := e.st.UserByIdentity(ctx, idp.srv.URL, "user-1")
	p, ok, _ := e.st.UserPicture(ctx, u.ID)
	if !ok || p.Source != store.PictureSSO || !bytes.Equal(p.Data, idp.picture) {
		t.Fatalf("sso picture = %v %+v", ok, p.Source)
	}
	if _, page := c.get("/settings/account"); !strings.Contains(page, "comes from SSO") {
		t.Error("the account page does not say where the picture comes from")
	}

	// An upload wins over the provider's picture.
	mine := testPNG(t, color.RGBA{10, 200, 10, 255})
	c.get("/settings/account")
	if resp := c.uploadPicture(mine); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	e.client().ssoLogin("/auth/oidc/login")
	if p, _, _ := e.st.UserPicture(ctx, u.ID); !bytes.Equal(p.Data, mine) {
		t.Error("the provider's picture replaced an uploaded one")
	}

	// Without an upload, a sign-in without a picture claim drops the SSO picture,
	// and a picture the provider will not serve is skipped.
	e.st.DeleteUserPicture(ctx, u.ID, store.PictureUpload)
	e.client().ssoLogin("/auth/oidc/login")
	idp.as(map[string]any{"sub": "user-1", "preferred_username": "alice"})
	e.client().ssoLogin("/auth/oidc/login")
	if _, ok, _ := e.st.UserPicture(ctx, u.ID); ok {
		t.Error("the SSO picture outlived its claim")
	}
	for _, pic := range []string{idp.srv.URL + "/missing.png", "ftp://id.example.com/p.png", "://bad"} {
		idp.as(map[string]any{"sub": "user-1", "preferred_username": "alice", "picture": pic})
		if resp, _ := e.client().ssoLogin("/auth/oidc/login"); resp.StatusCode != http.StatusSeeOther {
			t.Errorf("sign-in with picture %q: %d", pic, resp.StatusCode)
		}
	}
	if _, ok, _ := e.st.UserPicture(ctx, u.ID); ok {
		t.Error("stored a picture that could not be fetched")
	}
}

func TestRemovingTheSSOPictureSticks(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	idp := newIDP(t)
	configureSSO(t, e.admin(), idp, nil)
	idp.picture = testPNG(t, color.RGBA{40, 120, 200, 255})
	idp.as(map[string]any{"sub": "user-1", "preferred_username": "alice", "picture": idp.srv.URL + "/picture.png"})
	c := e.client()
	c.ssoLogin("/auth/oidc/login")
	id := e.userID("alice")

	if _, page := c.get("/settings/account"); !strings.Contains(page, "Remove picture") {
		t.Fatal("no Remove picture for a picture from SSO")
	}
	if resp, _ := c.htmx("/settings/picture/remove", url.Values{}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove: %d", resp.StatusCode)
	}
	e.client().ssoLogin("/auth/oidc/login")
	if u, _ := e.st.UserByID(ctx, id); u.Picture != 0 || u.SSOPicture {
		t.Fatalf("the SSO picture came back: picture=%d sso=%v", u.Picture, u.SSOPicture)
	}
	_, page := c.get("/settings/account")
	if !strings.Contains(page, "Use it again") || strings.Contains(page, "Remove picture") {
		t.Fatal("the account page does not offer the SSO picture back")
	}

	if resp, _ := c.htmx("/settings/picture/sso", url.Values{}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("use again: %d", resp.StatusCode)
	}
	e.client().ssoLogin("/auth/oidc/login")
	if u, _ := e.st.UserByID(ctx, id); u.Picture == 0 || !u.SSOPicture {
		t.Fatal("the SSO picture did not come back after Use it again")
	}
}

func TestStaticFilesAreVersioned(t *testing.T) {
	e := newEnv(t)
	admin := e.admin()
	_, page := e.client().get("/login")
	v := staticVersion()
	if len(v) != 12 || !strings.Contains(page, `/static/icon.png?v=`+v) {
		t.Fatalf("the sign-in page does not link versioned static files (v=%s)", v)
	}
	_, page = admin.get("/settings")
	for _, f := range []string{"app.css", "app.js", "htmx.min.js"} {
		if !strings.Contains(page, `/static/`+f+`?v=`+v) {
			t.Errorf("%s is not linked with ?v=", f)
		}
	}
	resp, _ := e.client().get("/static/app.css?v=" + v)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("versioned: %d %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	if resp, _ := e.client().get("/static/app.css"); resp.Header.Get("Cache-Control") != "public, max-age=3600" {
		t.Errorf("unversioned: %q", resp.Header.Get("Cache-Control"))
	}
}

func TestSSOPictureTokenStaysWithTheProvider(t *testing.T) {
	var auth string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Write(testPNG(t, color.Black))
	}))
	defer other.Close()
	e := newEnv(t)
	idp := newIDP(t)
	configureSSO(t, e.admin(), idp, nil)
	idp.as(map[string]any{"sub": "user-1", "preferred_username": "alice", "picture": other.URL + "/p.png"})
	e.client().ssoLogin("/auth/oidc/login")
	if auth != "" {
		t.Errorf("the access token went to another host: %q", auth)
	}
	u, _, _ := e.st.UserByIdentity(context.Background(), idp.srv.URL, "user-1")
	if u.Picture == 0 {
		t.Error("the picture from another host was not stored")
	}
}

func TestInitial(t *testing.T) {
	for in, want := range map[string]string{"alice": "A", "élodie": "É", "": "?"} {
		if got := initial(in); got != want {
			t.Errorf("initial(%q) = %q, want %q", in, got, want)
		}
	}
}
