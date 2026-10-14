// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"testing"
)

func TestUserPictures(t *testing.T) {
	ctx, s := context.Background(), open(t)
	a, _ := s.CreateUser(ctx, "admin", "test-password-123", RoleAdmin, t0)
	if _, ok, err := s.UserPicture(ctx, a.ID); ok || err != nil {
		t.Fatalf("picture before any: %v %v", ok, err)
	}

	if stored, err := s.SetUserPicture(ctx, a.ID, "image/png", []byte("sso"), PictureSSO, t0); err != nil || !stored {
		t.Fatalf("sso: %v %v", stored, err)
	}
	if u, _ := s.UserByID(ctx, a.ID); u.Picture != t0.Unix() {
		t.Errorf("User.Picture = %d, want %d", u.Picture, t0.Unix())
	}
	if stored, _ := s.SetUserPicture(ctx, a.ID, "image/png", []byte("mine"), PictureUpload, t0.Add(1)); !stored {
		t.Fatal("an upload did not replace the SSO picture")
	}
	if stored, _ := s.SetUserPicture(ctx, a.ID, "image/png", []byte("sso2"), PictureSSO, t0.Add(2)); stored {
		t.Fatal("the SSO picture replaced an upload")
	}
	p, _, _ := s.UserPicture(ctx, a.ID)
	if string(p.Data) != "mine" || p.Source != PictureUpload || p.ContentType != "image/png" {
		t.Fatalf("picture = %q from %s", p.Data, p.Source)
	}

	// Deleting by the wrong source keeps it.
	s.DeleteUserPicture(ctx, a.ID, PictureSSO)
	if _, ok, _ := s.UserPicture(ctx, a.ID); !ok {
		t.Fatal("deleting the SSO picture removed an upload")
	}
	if _, err := s.SetUserPicture(ctx, a.ID, "image/png", nil, "camera", t0); err == nil {
		t.Error("accepted an unknown source")
	}

	// The picture goes with its user.
	b, _ := s.CreateUser(ctx, "admin2", "test-password-123", RoleAdmin, t0)
	s.SetUserPicture(ctx, b.ID, "image/png", []byte("b"), PictureUpload, t0)
	if err := s.DeleteUser(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.UserPicture(ctx, b.ID); ok {
		t.Error("the picture outlived its user")
	}
}

func TestRemoveUserPicture(t *testing.T) {
	ctx, s := context.Background(), open(t)
	a, _ := s.CreateUser(ctx, "admin", "test-password-123", RoleAdmin, t0)
	s.SetUserPicture(ctx, a.ID, "image/png", []byte("mine"), PictureUpload, t0)

	// Removing an upload keeps SSO pictures allowed; removing one from SSO
	// turns them off until SetSSOPicture.
	if err := s.RemoveUserPicture(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.UserByID(ctx, a.ID); u.Picture != 0 || !u.SSOPicture {
		t.Fatalf("after removing an upload: %+v", u)
	}
	s.SetUserPicture(ctx, a.ID, "image/png", []byte("sso"), PictureSSO, t0)
	s.RemoveUserPicture(ctx, a.ID)
	if u, _ := s.UserByID(ctx, a.ID); u.Picture != 0 || u.SSOPicture {
		t.Fatalf("after removing an SSO picture: %+v", u)
	}
	if err := s.SetSSOPicture(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.UserByID(ctx, a.ID); !u.SSOPicture {
		t.Fatal("SetSSOPicture(true) did not stick")
	}
}
