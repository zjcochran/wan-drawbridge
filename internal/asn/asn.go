// SPDX-License-Identifier: AGPL-3.0-or-later

// Package asn maps a public IPv4 address to its autonomous system (the ISP).
// It uses the offline iptoasn.com database and falls back to ipinfo.io.
package asn

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Default sources: the iptoasn table, and ipinfo for single IPs the table
// cannot answer.
const (
	DefaultDBURL     = "https://iptoasn.com/data/ip2asn-v4.tsv.gz"
	DefaultOnlineURL = "https://ipinfo.io/%s/json"
)

// Info identifies an ISP.
type Info struct {
	ASN  int
	Name string
}

type span struct {
	start, end uint32
	asn        int
	name       string
}

// DB is an in-memory, sorted table of IPv4 ranges.
type DB struct{ spans []span }

// Parse reads the iptoasn TSV format:
// range_start  range_end  AS_number  country_code  AS_description
func Parse(r io.Reader) (*DB, error) {
	db := &DB{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), "\t", 5)
		if len(f) < 5 {
			continue
		}
		asn, err := strconv.Atoi(f[2])
		if err != nil || asn == 0 { // 0 = not routed
			continue
		}
		start, err1 := netip.ParseAddr(f[0])
		end, err2 := netip.ParseAddr(f[1])
		if err1 != nil || err2 != nil || !start.Is4() || !end.Is4() {
			continue
		}
		db.spans = append(db.spans, span{u32(start), u32(end), asn, f[4]})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(db.spans) == 0 {
		return nil, errors.New("asn database is empty")
	}
	sort.Slice(db.spans, func(i, j int) bool { return db.spans[i].start < db.spans[j].start })
	return db, nil
}

// Lookup returns the ASN that announces ip.
func (db *DB) Lookup(ip netip.Addr) (Info, bool) {
	if db == nil || !ip.Is4() {
		return Info{}, false
	}
	v := u32(ip)
	i := sort.Search(len(db.spans), func(i int) bool { return db.spans[i].start > v }) - 1
	if i < 0 || v > db.spans[i].end {
		return Info{}, false
	}
	s := db.spans[i]
	return Info{ASN: s.asn, Name: s.name}, true
}

// Len returns the number of address ranges in the database.
func (db *DB) Len() int { return len(db.spans) }

func u32(ip netip.Addr) uint32 {
	b := ip.As4()
	return binary.BigEndian.Uint32(b[:])
}

// Resolver combines the offline DB, an online fallback and a per-IP cache.
type Resolver struct {
	Path      string        // where the gzipped TSV is stored
	DBURL     string        // download source
	MaxAge    time.Duration // refresh when older than this
	OnlineURL string        // fmt pattern with one %s for the IP; "" disables
	Log       *slog.Logger

	client *http.Client
	mu     sync.RWMutex
	db     *DB
	cache  map[netip.Addr]Info
}

// NewResolver returns a Resolver that keeps the ASN database in cacheDir,
// downloads it again when it is a week old, and falls back to ipinfo.
func NewResolver(cacheDir string, log *slog.Logger) *Resolver {
	return &Resolver{
		Path:      filepath.Join(cacheDir, "ip2asn-v4.tsv.gz"),
		DBURL:     DefaultDBURL,
		MaxAge:    7 * 24 * time.Hour,
		OnlineURL: DefaultOnlineURL,
		Log:       log,
		client:    &http.Client{Timeout: 60 * time.Second},
		cache:     map[netip.Addr]Info{},
	}
}

// Refresh downloads the database if it is missing or stale, then loads it.
// A failed download keeps whatever copy is already loaded or on disk.
func (r *Resolver) Refresh(ctx context.Context) error {
	r.mu.RLock()
	loaded := r.db != nil
	r.mu.RUnlock()

	st, statErr := os.Stat(r.Path)
	if statErr != nil || time.Since(st.ModTime()) > r.MaxAge {
		err := r.download(ctx)
		if err == nil {
			return r.load()
		}
		r.Log.Warn("asn database download failed", "err", err)
		if statErr != nil { // nothing on disk to fall back to
			return err
		}
	}
	if loaded {
		return nil
	}
	return r.load()
}

func (r *Resolver) download(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.DBURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "wan-drawbridge")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	tmp := r.Path + ".tmp"
	f, err := os.Create(tmp) //nolint:gosec // G304: the path comes from DRAWBRIDGE_CACHE_DIR, not from a request
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	r.Log.Info("asn database downloaded", "path", r.Path)
	return os.Rename(tmp, r.Path)
}

func (r *Resolver) load() error {
	f, err := os.Open(r.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	db, err := Parse(gz)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.db = db
	r.cache = map[netip.Addr]Info{}
	r.mu.Unlock()
	r.Log.Info("asn database loaded", "ranges", db.Len())
	return nil
}

// Lookup resolves ip using the cache, the offline DB, then the online fallback.
func (r *Resolver) Lookup(ctx context.Context, ip netip.Addr) (Info, error) {
	r.mu.RLock()
	info, ok := r.cache[ip]
	db := r.db
	r.mu.RUnlock()
	if ok {
		return info, nil
	}
	info, ok = db.Lookup(ip)
	if !ok {
		var err error
		if info, err = r.online(ctx, ip); err != nil {
			return Info{}, err
		}
	}
	r.mu.Lock()
	r.cache[ip] = info
	r.mu.Unlock()
	return info, nil
}

func (r *Resolver) online(ctx context.Context, ip netip.Addr) (Info, error) {
	if r.OnlineURL == "" {
		return Info{}, fmt.Errorf("no ASN found for %s", ip)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(r.OnlineURL, ip), nil)
	if err != nil {
		return Info{}, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Info{}, fmt.Errorf("online lookup: status %d", resp.StatusCode)
	}
	var body struct {
		Org string `json:"org"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&body); err != nil {
		return Info{}, err
	}
	return ParseOrg(body.Org)
}

// ParseOrg parses ipinfo's "AS64500 Examplenet Communications, LLC".
func ParseOrg(org string) (Info, error) {
	num, name, _ := strings.Cut(org, " ")
	n, err := strconv.Atoi(strings.TrimPrefix(num, "AS"))
	if !strings.HasPrefix(num, "AS") || err != nil || n == 0 {
		return Info{}, fmt.Errorf("unrecognised org %q", org)
	}
	return Info{ASN: n, Name: name}, nil
}
