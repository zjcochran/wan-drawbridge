// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpen(t *testing.T) {
	b, _ := New("k1")
	sealed, err := b.Seal("client-secret")
	if err != nil || sealed == "client-secret" {
		t.Fatalf("seal: %q %v", sealed, err)
	}
	if got, err := b.Open(sealed); err != nil || got != "client-secret" {
		t.Fatalf("open: %q %v", got, err)
	}
	other, _ := New("k2")
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("opened with the wrong key")
	}
	if s, _ := b.Seal(""); s != "" {
		t.Fatal("empty should stay empty")
	}
}

func TestKeyFileCreatedOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	a, err := Load("", path)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}
	sealed, _ := a.Seal("x")
	b, _ := Load("", path)
	if got, err := b.Open(sealed); err != nil || got != "x" {
		t.Fatal("reloaded key differs")
	}
}

func TestEnvKeyWinsOverFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	a, err := Load("from-env", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("no key file should be written when the key comes from the environment")
	}
	sealed, _ := a.Seal("x")
	b, _ := New("from-env")
	if got, err := b.Open(sealed); err != nil || got != "x" {
		t.Fatalf("same key should open it: %q %v", got, err)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.key")
	os.WriteFile(empty, []byte("\n"), 0o600)
	for name, path := range map[string]string{
		"empty key file":   empty,
		"unreadable":       dir, // a directory
		"cannot be stored": filepath.Join(dir, "missing", "secret.key"),
	} {
		if _, err := Load("", path); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestOpenRejectsMalformed(t *testing.T) {
	b, _ := New("k")
	if got, err := b.Open(""); err != nil || got != "" {
		t.Fatalf("empty should stay empty: %q %v", got, err)
	}
	for _, bad := range []string{"plain", "v1:not base64!", "v1:AAAA", "dmFsdWU="} {
		if _, err := b.Open(bad); err == nil {
			t.Errorf("Open(%q) should fail", bad)
		}
	}
}
