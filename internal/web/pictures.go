// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// MaxPictureBytes is the largest profile picture accepted, uploaded or fetched.
const MaxPictureBytes = 1 << 20

// pictureFetchTimeout bounds fetching a picture from the SSO provider.
const pictureFetchTimeout = 10 * time.Second

// pictureTypes are the image types accepted, by sniffed content type. SVG is
// not one: served from this origin it could run script.
var pictureTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

var errPictureTooBig = fmt.Errorf("the picture is larger than %d KB", MaxPictureBytes/1024)

// readPicture reads an image of at most MaxPictureBytes and returns its
// sniffed content type.
func readPicture(r io.Reader) (contentType string, data []byte, err error) {
	data, err = io.ReadAll(io.LimitReader(r, MaxPictureBytes+1))
	switch {
	case err != nil:
		return "", nil, err
	case len(data) > MaxPictureBytes:
		return "", nil, errPictureTooBig
	}
	contentType = http.DetectContentType(data)
	if !pictureTypes[contentType] {
		return "", nil, errors.New("the picture must be a PNG, JPEG, GIF or WebP image")
	}
	return contentType, data, nil
}

// refreshSSOPicture stores the picture from the provider's picture claim, or
// drops the stored SSO picture if the claim is empty. An uploaded picture is
// kept either way. Failures are logged: the sign-in goes on without it.
func (s *Server) refreshSSOPicture(ctx context.Context, userID int, l ssoLogin) {
	if err := s.fetchSSOPicture(ctx, userID, l); err != nil {
		s.Log.Warn("cannot fetch the sso profile picture", "user_id", userID, "url", l.claims.Picture, "err", err)
	}
}

func (s *Server) fetchSSOPicture(ctx context.Context, userID int, l ssoLogin) error {
	u, err := s.Store.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if !u.SSOPicture {
		return nil // the user removed it
	}
	if l.claims.Picture == "" {
		return s.Store.DeleteUserPicture(ctx, userID, store.PictureSSO)
	}
	pu, err := url.Parse(l.claims.Picture)
	if err != nil || (pu.Scheme != "https" && pu.Scheme != "http") || pu.Host == "" {
		return errors.New("the picture claim is not an http(s) URL")
	}
	ctx, cancel := context.WithTimeout(ctx, pictureFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pu.String(), nil)
	if err != nil {
		return err
	}
	// The provider's own picture URLs may need the access token; never send it elsewhere.
	if iss, err := url.Parse(l.issuer); err == nil && iss.Scheme == pu.Scheme && iss.Host == pu.Host && l.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+l.accessToken)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the URL is from the provider's signed ID token
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	ct, data, err := readPicture(resp.Body)
	if err != nil {
		return err
	}
	_, err = s.Store.SetUserPicture(ctx, userID, ct, data, store.PictureSSO, s.now())
	return err
}

// userPicture serves a profile picture to its owner and to admins.
func (s *Server) userPicture(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	me := sessionFrom(r).User
	if err != nil || (id != me.ID && !me.IsAdmin()) {
		http.NotFound(w, r)
		return
	}
	p, ok, err := s.Store.UserPicture(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", p.ContentType)
	// Links carry ?v=<updated_at>, so a new picture is a new URL.
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(p.Data) //nolint:errcheck // nothing to do if the client went away
}

// uploadPicture sets the signed-in user's own picture.
func (s *Server) uploadPicture(w http.ResponseWriter, r *http.Request) {
	f, _, err := r.FormFile("picture")
	if err != nil {
		invalid(w, "Choose a picture to upload.")
		return
	}
	defer f.Close()
	ct, data, err := readPicture(f)
	if err != nil {
		invalid(w, err.Error())
		return
	}
	me := sessionFrom(r).User
	if _, err := s.Store.SetUserPicture(r.Context(), me.ID, ct, data, store.PictureUpload, s.now()); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/settings/account?flash="+url.QueryEscape("Picture saved."))
}

// removePicture removes the signed-in user's picture. Removing one from
// single sign-on also stops single sign-on from setting it again.
func (s *Server) removePicture(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.RemoveUserPicture(r.Context(), sessionFrom(r).User.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/settings/account?flash="+url.QueryEscape("Picture removed."))
}

// useSSOPicture lets single sign-on set the signed-in user's picture again,
// from their next SSO sign-in.
func (s *Server) useSSOPicture(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.SetSSOPicture(r.Context(), sessionFrom(r).User.ID, true); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/settings/account?flash="+url.QueryEscape("Your picture will come from single sign-on from your next SSO sign-in."))
}
