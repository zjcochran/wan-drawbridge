// SPDX-License-Identifier: AGPL-3.0-or-later

package agentauth

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Unix(1_800_000_000, 0)

func newKey(t *testing.T) Key {
	t.Helper()
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestKeys(t *testing.T) {
	k := newKey(t)
	back, err := ParseKey(k.String())
	if err != nil || !back.Public().Equal(k.Public()) {
		t.Fatalf("private key round trip: %v", err)
	}
	pub, err := ParsePublicKey(" " + k.Public().String() + "\n")
	if err != nil || !pub.Equal(k.Public()) || !strings.HasPrefix(pub.String(), "ed25519:") {
		t.Fatalf("public key round trip: %v", err)
	}
	if len(pub.Fingerprint()) != 16 {
		t.Errorf("fingerprint %q", pub.Fingerprint())
	}
	for _, bad := range []string{"", "ed25519:", "ed25519:!!!", "rsa:AAAA", k.String()} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Errorf("public key %q accepted", bad)
		}
	}
	for _, bad := range []string{"", "ed25519-private:AAAA", k.Public().String()} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("private key %q accepted", bad)
		}
	}
	if PublicKey(nil).Verify([]byte("x"), k.Sign([]byte("x"))) {
		t.Error("an empty key verified")
	}
}

func TestRequests(t *testing.T) {
	k, other := newKey(t), newKey(t)
	h, err := SignRequest(k, 7, "POST", "/api/agent/v1/results", []byte(`[]`), t0)
	if err != nil {
		t.Fatal(err)
	}
	a, err := ParseAuth(h)
	if err != nil || a.Host != 7 || a.TS != t0.Unix() || a.Nonce == "" {
		t.Fatalf("ParseAuth = %+v, %v", a, err)
	}
	keys := []PublicKey{other.Public(), k.Public()}
	if used, err := a.Verify(keys, "POST", "/api/agent/v1/results", []byte(`[]`), t0.Add(time.Minute)); err != nil || !used.Equal(k.Public()) {
		t.Fatalf("Verify = %v", err)
	}
	for name, check := range map[string]func() error{
		"other key": func() error {
			_, err := a.Verify([]PublicKey{other.Public()}, "POST", "/api/agent/v1/results", []byte(`[]`), t0)
			return err
		},
		"other body": func() error {
			_, err := a.Verify(keys, "POST", "/api/agent/v1/results", []byte(`[{}]`), t0)
			return err
		},
		"other path": func() error {
			_, err := a.Verify(keys, "POST", "/api/agent/v1/inventory", []byte(`[]`), t0)
			return err
		},
		"other method": func() error {
			_, err := a.Verify(keys, "GET", "/api/agent/v1/results", []byte(`[]`), t0)
			return err
		},
		"too old": func() error {
			_, err := a.Verify(keys, "POST", "/api/agent/v1/results", []byte(`[]`), t0.Add(6*time.Minute))
			return err
		},
		"from the future": func() error {
			_, err := a.Verify(keys, "POST", "/api/agent/v1/results", []byte(`[]`), t0.Add(-6*time.Minute))
			return err
		},
	} {
		if check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, bad := range []string{"", "Bearer x", Scheme + " host=1", Scheme + " host=x, ts=1, nonce=a, sig=b",
		Scheme + " host=1, ts=y, nonce=a, sig=b", Scheme + " host=1, host=1, nonce=a, sig=b", Scheme + " host=1 ts=1"} {
		if _, err := ParseAuth(bad); err == nil {
			t.Errorf("header %q accepted", bad)
		}
	}
}

func TestNonces(t *testing.T) {
	var n Nonces
	if !n.Use("a", t0) || n.Use("a", t0.Add(time.Minute)) {
		t.Fatal("a nonce was accepted twice")
	}
	if !n.Use("a", t0.Add(NonceTTL+time.Second)) {
		t.Error("an old nonce is still remembered")
	}
}

func TestCommands(t *testing.T) {
	k, other := newKey(t), newKey(t)
	c, err := NewCommand(k, "k1", 7, CmdStop, "abc", StopArgs{TimeoutSeconds: 30}, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Check(k.Public(), 7, t0); err != nil {
		t.Fatal(err)
	}
	if string(c.Args) != `{"timeout_seconds":30}` || c.KeyID != "k1" || len(c.ID) != 32 {
		t.Errorf("command = %+v", c)
	}
	tampered := c
	tampered.Workload = "def"
	for name, err := range map[string]error{
		"other key":  c.Check(other.Public(), 7, t0),
		"other host": c.Check(k.Public(), 8, t0),
		"expired":    c.Check(k.Public(), 7, t0.Add(2*time.Minute)),
		"tampered":   tampered.Check(k.Public(), 7, t0),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := NewCommand(k, "k1", 7, CmdStop, "", make(chan int), t0); err == nil {
		t.Error("arguments that cannot be encoded were accepted")
	}
	noArgs, _ := NewCommand(k, "k1", 7, CmdStats, "", nil, t0.Add(time.Minute))
	if noArgs.Args != nil || noArgs.Check(k.Public(), 7, t0) != nil {
		t.Errorf("command without arguments = %+v", noArgs)
	}
}

func TestRotationProof(t *testing.T) {
	k := newKey(t)
	p := RotationProof(7, k.Public(), t0.Unix())
	if !k.Public().Verify(p, k.Sign(p)) || string(RotationProof(8, k.Public(), t0.Unix())) == string(p) {
		t.Error("proof does not bind the host")
	}
}
