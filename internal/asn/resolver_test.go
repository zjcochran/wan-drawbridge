// SPDX-License-Identifier: AGPL-3.0-or-later

package asn

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

// fakeSources serves the database and ipinfo-style lookups and counts requests.
type fakeSources struct {
	db       []byte
	dbStatus int
	dbHits   atomic.Int32
	orgHits  atomic.Int32
}

func (f *fakeSources) start(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/db.tsv.gz":
			f.dbHits.Add(1)
			if f.dbStatus != 0 {
				w.WriteHeader(f.dbStatus)
				return
			}
			w.Write(f.db)
		case "/8.8.8.8/json":
			f.orgHits.Add(1)
			io.WriteString(w, `{"org":"AS15169 Google LLC"}`)
		case "/9.9.9.9/json":
			io.WriteString(w, `{"org":"no AS here"}`)
		case "/4.4.4.4/json":
			io.WriteString(w, `not json`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// failingTransport fails every request without touching the network.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

func newTestResolver(t *testing.T, srv *httptest.Server) *Resolver {
	r := NewResolver(t.TempDir(), slog.New(slog.DiscardHandler))
	r.DBURL = srv.URL + "/db.tsv.gz"
	r.OnlineURL = srv.URL + "/%s/json"
	return r
}

func TestNewResolverDefaults(t *testing.T) {
	r := NewResolver("/cache", slog.New(slog.DiscardHandler))
	if r.Path != filepath.Join("/cache", "ip2asn-v4.tsv.gz") || r.DBURL != DefaultDBURL ||
		r.OnlineURL != DefaultOnlineURL || r.MaxAge != 7*24*time.Hour {
		t.Fatalf("unexpected defaults: %+v", r)
	}
}

func TestRefreshDownloadsThenUsesCache(t *testing.T) {
	src := &fakeSources{db: gzipped(t, sample)}
	r := newTestResolver(t, src.start(t))
	ctx := context.Background()

	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := r.Lookup(ctx, netip.MustParseAddr("203.0.113.3"))
	if err != nil || info.ASN != 64500 || info.Name != "EXAMPLENET-64500" {
		t.Fatalf("offline lookup: %+v, %v", info, err)
	}

	// Fresh on disk and loaded: no second download.
	if err := r.Refresh(ctx); err != nil || src.dbHits.Load() != 1 {
		t.Fatalf("refresh of a fresh database downloaded again: hits %d, %v", src.dbHits.Load(), err)
	}

	// A new resolver with the file already on disk loads it without downloading.
	r2 := newTestResolver(t, src.start(t))
	r2.Path = r.Path
	if err := r2.Refresh(ctx); err != nil || src.dbHits.Load() != 1 {
		t.Fatalf("file on disk was downloaded again: hits %d, %v", src.dbHits.Load(), err)
	}
	if _, err := r2.Lookup(ctx, netip.MustParseAddr("1.0.0.1")); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshKeepsStaleCopyWhenDownloadFails(t *testing.T) {
	src := &fakeSources{db: gzipped(t, sample)}
	r := newTestResolver(t, src.start(t))
	ctx := context.Background()
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(r.Path, old, old)
	src.dbStatus = http.StatusBadGateway

	if err := r.Refresh(ctx); err != nil {
		t.Fatalf("a failed refresh with a copy on disk should not fail: %v", err)
	}
	if src.dbHits.Load() != 2 {
		t.Fatalf("stale database was not re-downloaded: hits %d", src.dbHits.Load())
	}
	if _, err := r.Lookup(ctx, netip.MustParseAddr("203.0.113.3")); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshFailsWithNothingOnDisk(t *testing.T) {
	src := &fakeSources{dbStatus: http.StatusNotFound}
	r := newTestResolver(t, src.start(t))
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	r.client.Transport = failingTransport{}
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatal("expected an error for an unreachable server")
	}
	r.DBURL = "://bad"
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatal("expected an error for a bad URL")
	}
}

func TestRefreshRejectsCorruptDatabase(t *testing.T) {
	for name, body := range map[string][]byte{
		"not gzip": []byte("plain text"),
		"empty":    gzipped(t, "malformed line\n"),
	} {
		src := &fakeSources{db: body}
		r := newTestResolver(t, src.start(t))
		if err := r.Refresh(context.Background()); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	r := newTestResolver(t, (&fakeSources{}).start(t))
	r.Path = filepath.Join(t.TempDir(), "missing", "dir", "db.gz")
	if err := r.Refresh(context.Background()); err == nil {
		t.Error("expected an error when the cache directory is missing")
	}
}

func TestLookupFallsBackOnlineAndCaches(t *testing.T) {
	src := &fakeSources{db: gzipped(t, sample)}
	r := newTestResolver(t, src.start(t))
	ctx := context.Background()
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		info, err := r.Lookup(ctx, netip.MustParseAddr("8.8.8.8"))
		if err != nil || info.ASN != 15169 || info.Name != "Google LLC" {
			t.Fatalf("online lookup: %+v, %v", info, err)
		}
	}
	if src.orgHits.Load() != 1 {
		t.Fatalf("second lookup was not cached: %d online requests", src.orgHits.Load())
	}
	for _, ip := range []string{"9.9.9.9", "4.4.4.4", "5.5.5.5"} {
		if _, err := r.Lookup(ctx, netip.MustParseAddr(ip)); err == nil {
			t.Errorf("Lookup(%s) should fail", ip)
		}
	}
	r.OnlineURL = ""
	if _, err := r.Lookup(ctx, netip.MustParseAddr("6.6.6.6")); err == nil {
		t.Error("lookup with the online fallback disabled should fail")
	}
	r.OnlineURL = "http://ipinfo.invalid/%s"
	r.client.Transport = failingTransport{}
	if _, err := r.Lookup(ctx, netip.MustParseAddr("6.6.6.6")); err == nil {
		t.Error("lookup against an unreachable fallback should fail")
	}
	r.OnlineURL = "://bad/%s"
	if _, err := r.Lookup(ctx, netip.MustParseAddr("6.6.6.6")); err == nil {
		t.Error("lookup with a bad fallback URL should fail")
	}
}
