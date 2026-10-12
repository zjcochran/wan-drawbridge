// SPDX-License-Identifier: AGPL-3.0-or-later

package backup

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/store"
)

const pass = "correct horse battery"

func sample() (Meta, store.Snapshot) {
	meta := Meta{AppVersion: "v1.2.3", Schema: 5, Created: time.Unix(1_800_000_000, 0).UTC(), History: "30d"}
	snap := store.Snapshot{
		Config:  map[string][]store.Row{"users": {{"id": 1, "username": "alice"}}},
		History: map[string][]store.Row{"checks": {{"id": 7, "ts": 1_800_000_000}}},
	}
	return meta, snap
}

func encode(t *testing.T) []byte {
	t.Helper()
	meta, snap := sample()
	var buf bytes.Buffer
	if err := Encode(&buf, meta, snap, pass); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRoundTrip(t *testing.T) {
	b := encode(t)
	if bytes.Contains(b, []byte("alice")) {
		t.Fatal("backup data is readable without the passphrase")
	}
	meta, snap, err := Decode(bytes.NewReader(b), pass)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := sample()
	if meta != want {
		t.Fatalf("meta = %+v", meta)
	}
	if snap.Config["users"][0]["username"] != "alice" || snap.Config["users"][0]["id"] != json.Number("1") {
		t.Fatalf("config = %v", snap.Config)
	}
	if snap.History["checks"][0]["ts"] != json.Number("1800000000") {
		t.Fatalf("history = %v", snap.History)
	}
}

func TestShortPassphrase(t *testing.T) {
	meta, snap := sample()
	if err := Encode(&bytes.Buffer{}, meta, snap, "short"); err == nil {
		t.Fatal("short passphrase accepted")
	}
}

func TestWrongPassphraseOrDamage(t *testing.T) {
	b := encode(t)
	if _, _, err := Decode(bytes.NewReader(b), "wrong passphrase!"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	var f file
	json.Unmarshal(b, &f)
	f.Data[len(f.Data)/2] ^= 1
	damaged, _ := json.Marshal(f)
	if _, _, err := Decode(bytes.NewReader(damaged), pass); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("damaged: %v", err)
	}
}

// rewrite changes a valid file's envelope.
func rewrite(t *testing.T, change func(*file)) []byte {
	t.Helper()
	var f file
	if err := json.Unmarshal(encode(t), &f); err != nil {
		t.Fatal(err)
	}
	change(&f)
	b, _ := json.Marshal(f)
	return b
}

func TestRejectsOtherFiles(t *testing.T) {
	cases := map[string][]byte{
		"not json":      []byte("PK\x03\x04 a zip file"),
		"other json":    []byte(`{"hello":"world"}`),
		"other kdf":     rewrite(t, func(f *file) { f.KDF.Name = "pbkdf2" }),
		"huge memory":   rewrite(t, func(f *file) { f.KDF.Memory = 4 << 20 }),
		"many passes":   rewrite(t, func(f *file) { f.KDF.Time = 1000 }),
		"no threads":    rewrite(t, func(f *file) { f.KDF.Threads = 0 }),
		"short salt":    rewrite(t, func(f *file) { f.KDF.Salt = f.KDF.Salt[:4] }),
		"short nonce":   rewrite(t, func(f *file) { f.Nonce = f.Nonce[:4] }),
		"tiny memory":   rewrite(t, func(f *file) { f.KDF.Memory = 1 }),
		"zero time":     rewrite(t, func(f *file) { f.KDF.Time = 0 }),
		"many threads":  rewrite(t, func(f *file) { f.KDF.Threads = 64 }),
		"wrong format":  rewrite(t, func(f *file) { f.Format = "something-else" }),
		"empty payload": rewrite(t, func(f *file) { f.Data = nil }),
	}
	for name, b := range cases {
		_, _, err := Decode(bytes.NewReader(b), pass)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	future := rewrite(t, func(f *file) { f.Version = 2 })
	if _, _, err := Decode(bytes.NewReader(future), pass); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("newer format: %v", err)
	}
}

// seal encrypts plain the way Encode does, so tests can craft payloads.
func seal(t *testing.T, plain []byte) []byte {
	t.Helper()
	k := KDF{Name: "argon2id", Salt: make([]byte, 16), Time: 1, Memory: 64, Threads: 1}
	rand.Read(k.Salt)
	aead, err := newAEAD(pass, k)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	b, _ := json.Marshal(file{Format: FormatName, Version: Version, KDF: k, Nonce: nonce,
		Data: aead.Seal(nil, nonce, plain, []byte(FormatName))})
	return b
}

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(s))
	zw.Close()
	return buf.Bytes()
}

func TestDamagedPayload(t *testing.T) {
	for name, plain := range map[string][]byte{
		"not gzip":   []byte("plain text"),
		"not json":   gz(t, "not json"),
		"no config":  gz(t, `{"meta":{},"data":{}}`),
		"wrong type": gz(t, `{"meta":{},"data":{"config":[1,2]}}`),
	} {
		if _, _, err := Decode(bytes.NewReader(seal(t, plain)), pass); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, _, err := Decode(bytes.NewReader(seal(t, gz(t, `{"meta":{"history":"none"},"data":{"config":{}}}`))), pass); err != nil {
		t.Errorf("minimal payload: %v", err)
	}
}

func TestHistoryRange(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		choice  string
		include bool
		since   time.Time
		label   string
	}{
		{"", false, time.Time{}, "none"},
		{"none", false, time.Time{}, "none"},
		{"30", true, now.AddDate(0, 0, -30), "30d"},
		{"60", true, now.AddDate(0, 0, -60), "60d"},
		{"90", true, now.AddDate(0, 0, -90), "90d"},
		{"all", true, time.Time{}, "all"},
	}
	for _, c := range cases {
		include, since, label, err := HistoryRange(c.choice, now)
		if err != nil || include != c.include || !since.Equal(c.since) || label != c.label {
			t.Errorf("%q: %v %v %q %v", c.choice, include, since, label, err)
		}
	}
	if _, _, _, err := HistoryRange("7", now); err == nil {
		t.Error("unknown range accepted")
	}
}
