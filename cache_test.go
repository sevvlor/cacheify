package cacheify

import (
	"context"
	"errors"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr bool
	}{
		{
			name:    "should error if path is not valid",
			cfg:     &Config{Path: "/foo/bar", MaxExpiry: 300, Cleanup: 600},
			wantErr: true,
		},
		{
			name:    "should error if maxExpiry <= 1",
			cfg:     &Config{Path: os.TempDir(), MaxExpiry: 1, Cleanup: 600},
			wantErr: true,
		},
		{
			name:    "should error if cleanup <= 1",
			cfg:     &Config{Path: os.TempDir(), MaxExpiry: 300, Cleanup: 1},
			wantErr: true,
		},
		{
			name:    "should be valid",
			cfg:     &Config{Path: os.TempDir(), MaxExpiry: 300, Cleanup: 600},
			wantErr: false,
		},
		{
			name:    "should error if maxHeaderPairs exceeds the on-disk format limit",
			cfg:     &Config{Path: os.TempDir(), MaxExpiry: 300, Cleanup: 600, MaxHeaderPairs: maxWireHeaderPairs + 1},
			wantErr: true,
		},
		{
			name:    "should error if maxHeaderKeyLen exceeds the on-disk format limit",
			cfg:     &Config{Path: os.TempDir(), MaxExpiry: 300, Cleanup: 600, MaxHeaderKeyLen: maxWireHeaderKeyLen + 1},
			wantErr: true,
		},
		{
			name:    "should error if maxHeaderValueLen exceeds the on-disk format limit",
			cfg:     &Config{Path: os.TempDir(), MaxExpiry: 300, Cleanup: 600, MaxHeaderValueLen: maxWireHeaderValueLen + 1},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(context.Background(), nil, test.cfg, "cacheify")

			if test.wantErr && err == nil {
				t.Fatal("expected error on bad regexp format")
			}
		})
	}
}

func TestCache_ServeHTTP(t *testing.T) {
	dir := createTempDir(t)

	next := func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Cache-Control", "max-age=20")
		rw.WriteHeader(http.StatusOK)
	}

	cfg := &Config{Path: dir, MaxExpiry: 10, Cleanup: 20, AddStatusHeader: true, MaxHeaderPairs: 2, MaxHeaderKeyLen: 30, MaxHeaderValueLen: 100}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/some/path", nil)
	rw := httptest.NewRecorder()

	c.ServeHTTP(rw, req)

	if state := rw.Header().Get("Cache-Status"); state != "miss" {
		t.Errorf("unexpected cache state: want \"miss\", got: %q", state)
	}

	rw = httptest.NewRecorder()

	c.ServeHTTP(rw, req)

	if state := rw.Header().Get("Cache-Status"); state != "hit" {
		t.Errorf("unexpected cache state: want \"hit\", got: %q", state)
	}
}

func TestCache_NoCacheOnSetCookie(t *testing.T) {
	dir := createTempDir(t)

	next := func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Cache-Control", "max-age=20")
		rw.Header().Set("Set-Cookie", "XSRF-TOKEN=deadbeef; Path=/; Secure; SameSite=Strict")
		rw.WriteHeader(http.StatusOK)
	}

	cfg := &Config{
		Path:               dir,
		MaxExpiry:          10,
		Cleanup:            20,
		AddStatusHeader:    true,
		NoCacheOnSetCookie: true,
		MaxHeaderPairs:     5,
		MaxHeaderKeyLen:    30,
		MaxHeaderValueLen:  200,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/session", nil)

	for i := 0; i < 2; i++ {
		rw := httptest.NewRecorder()
		c.ServeHTTP(rw, req)

		if state := rw.Header().Get("Cache-Status"); state != "miss" {
			t.Errorf("request %d: unexpected cache state: want \"miss\" (Set-Cookie must never be cached), got: %q", i, state)
		}

		if got := rw.Header().Get("Set-Cookie"); got == "" {
			t.Errorf("request %d: expected Set-Cookie to be delivered to the client, got none", i)
		}
	}
}

func TestCache_NoCacheOnAuthorization(t *testing.T) {
	dir := createTempDir(t)

	next := func(rw http.ResponseWriter, req *http.Request) {
		// Cache-Control: public is exactly the override RFC 7234 §3.2 grants
		// for Authorization-bearing requests - the case noCacheOnAuthorization
		// exists to block by default.
		rw.Header().Set("Cache-Control", "public, max-age=20")
		rw.WriteHeader(http.StatusOK)
	}

	cfg := &Config{
		Path:                   dir,
		MaxExpiry:              10,
		Cleanup:                20,
		AddStatusHeader:        true,
		NoCacheOnAuthorization: true,
		MaxHeaderPairs:         5,
		MaxHeaderKeyLen:        30,
		MaxHeaderValueLen:      200,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/api/me", nil)
	req.Header.Set("Authorization", "Bearer user-a-token")

	for i := 0; i < 2; i++ {
		rw := httptest.NewRecorder()
		c.ServeHTTP(rw, req)

		if state := rw.Header().Get("Cache-Status"); state != "miss" {
			t.Errorf("request %d: unexpected cache state: want \"miss\" (Authorization requests must never be cached), got: %q", i, state)
		}
	}
}

func TestCache_AuthorizationCacheableWhenDisabled(t *testing.T) {
	dir := createTempDir(t)

	next := func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Cache-Control", "public, max-age=20")
		rw.WriteHeader(http.StatusOK)
	}

	cfg := &Config{
		Path:                   dir,
		MaxExpiry:              10,
		Cleanup:                20,
		AddStatusHeader:        true,
		NoCacheOnAuthorization: false,
		MaxHeaderPairs:         5,
		MaxHeaderKeyLen:        30,
		MaxHeaderValueLen:      200,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/api/public-leaderboard", nil)
	req.Header.Set("Authorization", "Bearer some-token")

	rw := httptest.NewRecorder()
	c.ServeHTTP(rw, req)
	if state := rw.Header().Get("Cache-Status"); state != "miss" {
		t.Fatalf("first request: unexpected cache state: want \"miss\", got: %q", state)
	}

	rw = httptest.NewRecorder()
	c.ServeHTTP(rw, req)
	if state := rw.Header().Get("Cache-Status"); state != "hit" {
		t.Errorf("expected second request to be a cache hit when noCacheOnAuthorization is disabled and Cache-Control: public is set, got: %q", state)
	}
}

// TestCache_AuthorizationNeverServedFromAnonymousHit guards against a bug where
// NoCacheOnAuthorization only blocked *writing* an Authorization-bearing
// response to the cache. Because the cache key never includes the
// Authorization header, an earlier anonymous request could populate the
// cache for a URL, and a later Authorization-bearing request for that same
// URL would then receive that stale anonymous hit directly from GetStream -
// without ever reaching the write-time check, and without the backend ever
// seeing the authenticated request at all.
func TestCache_AuthorizationNeverServedFromAnonymousHit(t *testing.T) {
	dir := createTempDir(t)

	var backendCalls []string
	next := func(rw http.ResponseWriter, req *http.Request) {
		auth := req.Header.Get("Authorization")
		backendCalls = append(backendCalls, auth)
		rw.Header().Set("Cache-Control", "public, max-age=20")
		_, _ = rw.Write([]byte("response-for:" + auth))
	}

	cfg := &Config{
		Path:                   dir,
		MaxExpiry:              10,
		Cleanup:                20,
		AddStatusHeader:        true,
		NoCacheOnAuthorization: true,
		MaxHeaderPairs:         5,
		MaxHeaderKeyLen:        30,
		MaxHeaderValueLen:      200,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	anonReq := httptest.NewRequest(http.MethodGet, "http://localhost/shared-url", nil)

	// Anonymous request populates the cache.
	rw := httptest.NewRecorder()
	c.ServeHTTP(rw, anonReq)
	if state := rw.Header().Get("Cache-Status"); state != "miss" {
		t.Fatalf("anonymous request 1: want \"miss\", got: %q", state)
	}

	// A second anonymous request confirms it's actually cached.
	rw = httptest.NewRecorder()
	c.ServeHTTP(rw, anonReq)
	if state := rw.Header().Get("Cache-Status"); state != "hit" {
		t.Fatalf("anonymous request 2: want \"hit\", got: %q", state)
	}

	// An authenticated request to the SAME URL must bypass the cache
	// entirely - it must never receive the anonymous cached body, and the
	// backend must see it.
	authReq := httptest.NewRequest(http.MethodGet, "http://localhost/shared-url", nil)
	authReq.Header.Set("Authorization", "Bearer secret-token")

	rw = httptest.NewRecorder()
	c.ServeHTTP(rw, authReq)

	if state := rw.Header().Get("Cache-Status"); state != "miss" {
		t.Errorf("authenticated request: want \"miss\" (must bypass cache), got: %q", state)
	}
	if got, want := rw.Body.String(), "response-for:Bearer secret-token"; got != want {
		t.Errorf("authenticated request got the stale anonymous cached response instead of its own: got %q, want %q", got, want)
	}
	if len(backendCalls) != 2 || backendCalls[1] != "Bearer secret-token" {
		t.Errorf("expected backend to be called for the authenticated request, backend calls: %v", backendCalls)
	}
}

// TestCache_RangeRequestBypassesCache guards against a Range request ever
// populating or reading the cache: this plugin has no support for storing or
// replaying byte-range slices, so a Range request must always reach the
// backend directly and must never be cached.
func TestCache_RangeRequestBypassesCache(t *testing.T) {
	dir := createTempDir(t)

	var backendCalls int
	next := func(rw http.ResponseWriter, req *http.Request) {
		backendCalls++
		rw.Header().Set("Cache-Control", "public, max-age=20")
		rw.Header().Set("Accept-Ranges", "bytes")
		if req.Header.Get("Range") != "" {
			rw.Header().Set("Content-Range", "bytes 0-3/10")
			rw.WriteHeader(http.StatusPartialContent)
			_, _ = rw.Write([]byte("0123"))
			return
		}
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("0123456789"))
	}

	cfg := &Config{
		Path:              dir,
		MaxExpiry:         10,
		Cleanup:           20,
		AddStatusHeader:   true,
		MaxHeaderPairs:    5,
		MaxHeaderKeyLen:   30,
		MaxHeaderValueLen: 200,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	rangeReq := httptest.NewRequest(http.MethodGet, "http://localhost/big-file", nil)
	rangeReq.Header.Set("Range", "bytes=0-3")

	for i := 0; i < 2; i++ {
		rw := httptest.NewRecorder()
		c.ServeHTTP(rw, rangeReq)

		if state := rw.Header().Get("Cache-Status"); state != "miss" {
			t.Errorf("range request %d: want \"miss\" (must bypass cache), got: %q", i, state)
		}
		if rw.Code != http.StatusPartialContent {
			t.Errorf("range request %d: want status 206, got: %d", i, rw.Code)
		}
	}
	if backendCalls != 2 {
		t.Errorf("expected backend to be called for every range request, got %d calls", backendCalls)
	}

	// A normal, full request for the same URL must get the complete body,
	// never the partial slice from the range requests above.
	fullReq := httptest.NewRequest(http.MethodGet, "http://localhost/big-file", nil)
	rw := httptest.NewRecorder()
	c.ServeHTTP(rw, fullReq)

	if got, want := rw.Body.String(), "0123456789"; got != want {
		t.Errorf("full request got wrong body: got %q, want %q", got, want)
	}
	if rw.Code != http.StatusOK {
		t.Errorf("full request: want status 200, got: %d", rw.Code)
	}
}

// TestCache_PartialContentNeverCached guards against a 206 response ever
// being stored, even if Cache-Control would otherwise allow it: a later,
// full (non-Range) request must never receive a previously cached partial
// body.
func TestCache_PartialContentNeverCached(t *testing.T) {
	dir := createTempDir(t)

	next := func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Cache-Control", "public, max-age=20")
		rw.Header().Set("Content-Range", "bytes 0-3/10")
		rw.WriteHeader(http.StatusPartialContent)
		_, _ = rw.Write([]byte("0123"))
	}

	cfg := &Config{
		Path:              dir,
		MaxExpiry:         10,
		Cleanup:           20,
		AddStatusHeader:   true,
		MaxHeaderPairs:    5,
		MaxHeaderKeyLen:   30,
		MaxHeaderValueLen: 200,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	// No Range header at all here - simulates a backend that returns 206
	// independently of what was requested (or a proxy quirk). Even so, this
	// must never be cached.
	req := httptest.NewRequest(http.MethodGet, "http://localhost/oddball", nil)

	for i := 0; i < 2; i++ {
		rw := httptest.NewRecorder()
		c.ServeHTTP(rw, req)
		if state := rw.Header().Get("Cache-Status"); state != "miss" {
			t.Errorf("request %d: want \"miss\" (206 must never be cached), got: %q", i, state)
		}
	}
}

// TestCache_NotModifiedNeverCached guards against the cache-poisoning
// scenario where a conditional request's 304 (empty body) response gets
// stored and then served to a later, unconditional request as if it were
// the full resource.
func TestCache_NotModifiedNeverCached(t *testing.T) {
	dir := createTempDir(t)

	next := func(rw http.ResponseWriter, req *http.Request) {
		if req.Header.Get("If-None-Match") != "" {
			// A common, spec-compliant pattern: refresh freshness via
			// Cache-Control on the 304 itself.
			rw.Header().Set("Cache-Control", "public, max-age=20")
			rw.Header().Set("Etag", `"abc123"`)
			rw.WriteHeader(http.StatusNotModified)
			return
		}
		rw.Header().Set("Cache-Control", "public, max-age=20")
		rw.Header().Set("Etag", `"abc123"`)
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("the real content"))
	}

	cfg := &Config{
		Path:              dir,
		MaxExpiry:         10,
		Cleanup:           20,
		AddStatusHeader:   true,
		MaxHeaderPairs:    5,
		MaxHeaderKeyLen:   30,
		MaxHeaderValueLen: 200,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	// A conditional request that gets a 304 back - this must not poison the
	// cache for subsequent unconditional requests.
	conditionalReq := httptest.NewRequest(http.MethodGet, "http://localhost/asset.js", nil)
	conditionalReq.Header.Set("If-None-Match", `"abc123"`)

	rw := httptest.NewRecorder()
	c.ServeHTTP(rw, conditionalReq)
	if rw.Code != http.StatusNotModified {
		t.Fatalf("conditional request: want status 304, got: %d", rw.Code)
	}
	if state := rw.Header().Get("Cache-Status"); state != "miss" {
		t.Errorf("conditional request: want \"miss\" (304 must never be cached), got: %q", state)
	}

	// A normal, unconditional request for the same URL must get the real,
	// full content - never an empty body from the 304 above.
	plainReq := httptest.NewRequest(http.MethodGet, "http://localhost/asset.js", nil)
	rw = httptest.NewRecorder()
	c.ServeHTTP(rw, plainReq)

	if rw.Code != http.StatusOK {
		t.Errorf("unconditional request: want status 200, got: %d", rw.Code)
	}
	if got, want := rw.Body.String(), "the real content"; got != want {
		t.Errorf("unconditional request got wrong body: got %q, want %q", got, want)
	}
}

// TestCache_NoHeuristicCaching guards a response with no Cache-Control,
// Expires, or Public directive at all - e.g. an ordinary dynamic JSON API
// endpoint that never made any caching decision - is never cached when
// NoHeuristicCaching is enabled, even though its status code (200) is
// heuristically cacheable by default per RFC 7234 §4.2.2.
func TestCache_NoHeuristicCaching(t *testing.T) {
	dir := createTempDir(t)

	next := func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{"some":"data"}`))
	}

	cfg := &Config{
		Path:               dir,
		MaxExpiry:          10,
		Cleanup:            20,
		AddStatusHeader:    true,
		NoHeuristicCaching: true,
		MaxHeaderPairs:     5,
		MaxHeaderKeyLen:    30,
		MaxHeaderValueLen:  200,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/api/data", nil)

	for i := 0; i < 2; i++ {
		rw := httptest.NewRecorder()
		c.ServeHTTP(rw, req)
		if state := rw.Header().Get("Cache-Status"); state != "miss" {
			t.Errorf("request %d: want \"miss\" (headerless response must never be cached), got: %q", i, state)
		}
	}
}

// TestCache_NoHeuristicCaching_LastModifiedAlone guards against a response
// carrying ONLY a Last-Modified header (no Cache-Control, no Expires) from
// being cached when NoHeuristicCaching is enabled. The underlying
// cachecontrol library derives a non-zero expireBy from Last-Modified alone
// (Apache's "10% of time since last-modified" heuristic), so checking
// expireBy.IsZero() is not sufficient to detect the absence of an explicit
// freshness signal - this must be checked against the raw headers instead.
func TestCache_NoHeuristicCaching_LastModifiedAlone(t *testing.T) {
	dir := createTempDir(t)

	next := func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "text/javascript")
		rw.Header().Set("Last-Modified", time.Now().Add(-30*24*time.Hour).Format(http.TimeFormat))
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("console.log('hi')"))
	}

	cfg := &Config{
		Path:               dir,
		MaxExpiry:          10,
		Cleanup:            20,
		AddStatusHeader:    true,
		NoHeuristicCaching: true,
		MaxHeaderPairs:     5,
		MaxHeaderKeyLen:    30,
		MaxHeaderValueLen:  200,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/app.js", nil)

	for i := 0; i < 2; i++ {
		rw := httptest.NewRecorder()
		c.ServeHTTP(rw, req)
		if state := rw.Header().Get("Cache-Status"); state != "miss" {
			t.Errorf("request %d: want \"miss\" (Last-Modified alone must not trigger heuristic caching), got: %q", i, state)
		}
	}
}

// TestCache_HeuristicCachingWhenDisabled confirms disabling NoHeuristicCaching
// restores the pre-hardening v1.0.0 behaviour of caching a headerless
// response using MaxExpiry as its heuristic lifetime.
func TestCache_HeuristicCachingWhenDisabled(t *testing.T) {
	dir := createTempDir(t)

	next := func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("no headers at all"))
	}

	cfg := &Config{
		Path:               dir,
		MaxExpiry:          10,
		Cleanup:            20,
		AddStatusHeader:    true,
		NoHeuristicCaching: false,
		MaxHeaderPairs:     5,
		MaxHeaderKeyLen:    30,
		MaxHeaderValueLen:  200,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/legacy", nil)

	rw := httptest.NewRecorder()
	c.ServeHTTP(rw, req)
	if state := rw.Header().Get("Cache-Status"); state != "miss" {
		t.Fatalf("first request: want \"miss\", got: %q", state)
	}

	rw = httptest.NewRecorder()
	c.ServeHTTP(rw, req)
	if state := rw.Header().Get("Cache-Status"); state != "hit" {
		t.Errorf("expected second request to be a cache hit when noHeuristicCaching is disabled, got: %q", state)
	}
}

// TestCache_ExplicitCacheControlStillCachedWithNoHeuristicCaching confirms
// NoHeuristicCaching only blocks the headerless fallback path - a response
// with an explicit Cache-Control must still be cached normally.
func TestCache_ExplicitCacheControlStillCachedWithNoHeuristicCaching(t *testing.T) {
	dir := createTempDir(t)

	next := func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Cache-Control", "public, max-age=20")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("explicitly cacheable"))
	}

	cfg := &Config{
		Path:               dir,
		MaxExpiry:          10,
		Cleanup:            20,
		AddStatusHeader:    true,
		NoHeuristicCaching: true,
		MaxHeaderPairs:     5,
		MaxHeaderKeyLen:    30,
		MaxHeaderValueLen:  200,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/explicit", nil)

	rw := httptest.NewRecorder()
	c.ServeHTTP(rw, req)
	if state := rw.Header().Get("Cache-Status"); state != "miss" {
		t.Fatalf("first request: want \"miss\", got: %q", state)
	}

	rw = httptest.NewRecorder()
	c.ServeHTTP(rw, req)
	if state := rw.Header().Get("Cache-Status"); state != "hit" {
		t.Errorf("expected second request to be a cache hit (explicit Cache-Control present), got: %q", state)
	}
}

// TestCache_OnlyCacheGetAndHead_PostBypassesCache guards against the
// GraphQL-style scenario: a single POST endpoint that distinguishes requests
// purely by body. Since cacheify's cache key never incorporates the request
// body, a POST allowed to be cached (per RFC 7234, when explicit freshness
// is present) would let a later, different POST body receive the first
// request's stale response instead of reaching the backend at all.
func TestCache_OnlyCacheGetAndHead_PostBypassesCache(t *testing.T) {
	dir := createTempDir(t)

	next := func(rw http.ResponseWriter, req *http.Request) {
		body, _ := ioutil.ReadAll(req.Body)
		rw.Header().Set("Cache-Control", "public, max-age=20")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("response-for:" + string(body)))
	}

	cfg := &Config{
		Path:                dir,
		MaxExpiry:           10,
		Cleanup:             20,
		AddStatusHeader:     true,
		OnlyCacheGetAndHead: true,
		MaxHeaderPairs:      5,
		MaxHeaderKeyLen:     30,
		MaxHeaderValueLen:   200,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	postAlice := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://localhost/graphql", strings.NewReader("query-alice"))
		rw := httptest.NewRecorder()
		c.ServeHTTP(rw, req)
		return rw
	}
	postBob := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://localhost/graphql", strings.NewReader("query-bob"))
		rw := httptest.NewRecorder()
		c.ServeHTTP(rw, req)
		return rw
	}

	rw := postAlice()
	if state := rw.Header().Get("Cache-Status"); state != "miss" {
		t.Errorf("first POST: want \"miss\" (POST must bypass cache), got: %q", state)
	}
	if got, want := rw.Body.String(), "response-for:query-alice"; got != want {
		t.Errorf("first POST got wrong body: got %q, want %q", got, want)
	}

	// A second, different POST to the same URL must reach the backend and
	// get its OWN response - never Alice's cached one.
	rw = postBob()
	if state := rw.Header().Get("Cache-Status"); state != "miss" {
		t.Errorf("second POST: want \"miss\" (must bypass cache, not reuse the first POST's response), got: %q", state)
	}
	if got, want := rw.Body.String(), "response-for:query-bob"; got != want {
		t.Errorf("second POST got the wrong body (likely served Alice's cached response): got %q, want %q", got, want)
	}
}

// TestCache_PostCacheableWhenOnlyCacheGetAndHeadDisabled confirms disabling
// OnlyCacheGetAndHead restores the underlying library's default POST
// handling (cacheable given explicit freshness), demonstrating the exact
// risk the option exists to prevent: a second, different POST body to the
// same URL receives the first POST's stale cached response.
func TestCache_PostCacheableWhenOnlyCacheGetAndHeadDisabled(t *testing.T) {
	dir := createTempDir(t)

	next := func(rw http.ResponseWriter, req *http.Request) {
		body, _ := ioutil.ReadAll(req.Body)
		rw.Header().Set("Cache-Control", "public, max-age=20")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("response-for:" + string(body)))
	}

	cfg := &Config{
		Path:                dir,
		MaxExpiry:           10,
		Cleanup:             20,
		AddStatusHeader:     true,
		OnlyCacheGetAndHead: false,
		MaxHeaderPairs:      5,
		MaxHeaderKeyLen:     30,
		MaxHeaderValueLen:   200,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	aliceReq := httptest.NewRequest(http.MethodPost, "http://localhost/graphql", strings.NewReader("query-alice"))
	rw := httptest.NewRecorder()
	c.ServeHTTP(rw, aliceReq)
	if state := rw.Header().Get("Cache-Status"); state != "miss" {
		t.Fatalf("first POST: want \"miss\", got: %q", state)
	}

	bobReq := httptest.NewRequest(http.MethodPost, "http://localhost/graphql", strings.NewReader("query-bob"))
	rw = httptest.NewRecorder()
	c.ServeHTTP(rw, bobReq)
	if state := rw.Header().Get("Cache-Status"); state != "hit" {
		t.Fatalf("expected second POST to be a cache hit when onlyCacheGetAndHead is disabled, got: %q", state)
	}
	if got, want := rw.Body.String(), "response-for:query-alice"; got != want {
		t.Errorf("expected the (undesirable but library-default) stale cross-request response, got %q, want %q", got, want)
	}
}

func TestCache_VaryHandling(t *testing.T) {
	tests := []struct {
		name      string
		varyValue string
		wantCache bool
	}{
		{name: "no Vary header", varyValue: "", wantCache: true},
		{name: "Vary: Accept-Encoding is safelisted", varyValue: "Accept-Encoding", wantCache: true},
		{name: "Vary: Accept-Encoding, Accept-Language is safelisted", varyValue: "Accept-Encoding, Accept-Language", wantCache: true},
		{name: "Vary: Cookie must not be cached", varyValue: "Cookie", wantCache: false},
		{name: "Vary: Authorization must not be cached", varyValue: "Authorization", wantCache: false},
		{name: "Vary: * must never be cached", varyValue: "*", wantCache: false},
		{name: "mixed safelisted and unsafe still blocks caching", varyValue: "Accept-Encoding, Cookie", wantCache: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := createTempDir(t)

			next := func(rw http.ResponseWriter, req *http.Request) {
				rw.Header().Set("Cache-Control", "max-age=20")
				if test.varyValue != "" {
					rw.Header().Set("Vary", test.varyValue)
				}
				rw.WriteHeader(http.StatusOK)
			}

			cfg := &Config{
				Path:              dir,
				MaxExpiry:         10,
				Cleanup:           20,
				AddStatusHeader:   true,
				MaxHeaderPairs:    5,
				MaxHeaderKeyLen:   30,
				MaxHeaderValueLen: 200,
			}

			c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest(http.MethodGet, "http://localhost/vary-test", nil)

			rw := httptest.NewRecorder()
			c.ServeHTTP(rw, req)
			if state := rw.Header().Get("Cache-Status"); state != "miss" {
				t.Fatalf("first request: unexpected cache state: want \"miss\", got: %q", state)
			}

			rw = httptest.NewRecorder()
			c.ServeHTTP(rw, req)

			state := rw.Header().Get("Cache-Status")
			if test.wantCache && state != "hit" {
				t.Errorf("expected second request to be a cache hit, got: %q", state)
			}
			if !test.wantCache && state != "miss" {
				t.Errorf("expected second request to bypass the cache, got: %q", state)
			}
		})
	}
}

func TestCache_WebSocketUpgradeBypass(t *testing.T) {
	dir := createTempDir(t)

	var nextReceivedWriter http.ResponseWriter

	next := func(rw http.ResponseWriter, req *http.Request) {
		nextReceivedWriter = rw
		rw.WriteHeader(http.StatusSwitchingProtocols)
	}

	cfg := &Config{
		Path:              dir,
		MaxExpiry:         10,
		Cleanup:           20,
		AddStatusHeader:   true,
		MaxHeaderPairs:    2,
		MaxHeaderKeyLen:   30,
		MaxHeaderValueLen: 100,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	rw := httptest.NewRecorder()

	c.ServeHTTP(rw, req)

	// The next handler must receive the original ResponseWriter directly,
	// not our responseWriter wrapper — otherwise http.Hijacker will not be
	// available and Traefik's proxy will fail with "not a hijacker".
	if nextReceivedWriter != rw {
		t.Error("upgrade request was not passed through: next handler did not receive the original ResponseWriter")
	}

	// No Cache-Status header should be set for upgrade requests
	if state := rw.Header().Get("Cache-Status"); state != "" {
		t.Errorf("unexpected Cache-Status header on upgrade request: %q", state)
	}

	// Nothing should have been written to the cache directory
	verifyNoTempFiles(t, dir)
}

func TestCache_UpstreamFailureDuringStream(t *testing.T) {
	dir := createTempDir(t)

	// Simulate upstream server that fails mid-response
	next := func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Cache-Control", "max-age=20")
		rw.WriteHeader(http.StatusOK)

		// Write partial response
		_, _ = rw.Write([]byte("partial data"))

		// Simulate upstream failure by panicking
		// In a real scenario, this could be a network error, connection reset, etc.
		panic("upstream server failed mid-response")
	}

	cfg := &Config{
		Path:              dir,
		MaxExpiry:         10,
		Cleanup:           20,
		AddStatusHeader:   true,
		MaxHeaderPairs:    2,
		MaxHeaderKeyLen:   30,
		MaxHeaderValueLen: 100,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/some/path", nil)
	rw := httptest.NewRecorder()

	// Note: The panic may or may not propagate depending on the implementation
	// (for yaegi compatibility, we gracefully handle it rather than re-panicking)
	// The important test is whether partial data gets cached below
	func() {
		defer func() {
			_ = recover() // Silently catch if it panics
		}()
		c.ServeHTTP(rw, req)
	}()

	// The key question: is there a partial response cached?
	// Try to fetch from cache
	rw2 := httptest.NewRecorder()

	// Use a working backend for the second request
	workingNext := func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Cache-Control", "max-age=20")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("complete data from working backend"))
	}

	c2, err := New(context.Background(), http.HandlerFunc(workingNext), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	c2.ServeHTTP(rw2, req)

	// Check if we got a cache hit or miss
	cacheStatus := rw2.Header().Get("Cache-Status")
	t.Logf("Cache status on second request: %s", cacheStatus)
	t.Logf("Response body on second request: %s", rw2.Body.String())

	// We expect a cache MISS because the partial write should have been cleaned up
	// If we get a cache HIT with partial data, that's a bug!
	if cacheStatus == "hit" && rw2.Body.String() == "partial data" {
		t.Fatal("BUG: partial response was cached! This should not happen.")
	}

	// Verify no partial or temp files remain in the cache directory
	verifyNoTempFiles(t, dir)
}

func TestCache_DownstreamFailureDuringStream(t *testing.T) {
	dir := createTempDir(t)

	// Upstream server that works fine
	next := func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Cache-Control", "max-age=20")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("complete response data"))
	}

	cfg := &Config{
		Path:              dir,
		MaxExpiry:         10,
		Cleanup:           20,
		AddStatusHeader:   true,
		MaxHeaderPairs:    2,
		MaxHeaderKeyLen:   30,
		MaxHeaderValueLen: 100,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/some/path", nil)

	// Create a ResponseWriter that simulates downstream failure
	failingWriter := &failingResponseWriter{
		ResponseWriter: httptest.NewRecorder(),
		failAfterBytes: 10, // Fail after writing 10 bytes
	}

	// Serve the request with a failing downstream
	// The error won't be returned directly (ServeHTTP doesn't return errors)
	// but we can check if the cache was properly aborted
	c.ServeHTTP(failingWriter, req)

	t.Logf("First request completed (downstream failed during write)")

	// The key question: is there a partial response cached?
	// Try to fetch from cache with a working client
	// This will HANG if the lock was not released!
	t.Log("Attempting second request (will hang if lock not released)...")

	done := make(chan bool)
	go func() {
		rw2 := httptest.NewRecorder()
		c.ServeHTTP(rw2, req)

		// Check if we got a cache hit or miss
		cacheStatus := rw2.Header().Get("Cache-Status")
		t.Logf("Cache status on second request: %s", cacheStatus)
		t.Logf("Response body on second request: %s", rw2.Body.String())

		// Question: Should this be a hit or miss?
		// - If MISS: downstream failure prevented caching (safest)
		// - If HIT with full data: cache succeeded despite downstream failure (acceptable)
		// - If HIT with partial data: BUG!

		if cacheStatus == "hit" && rw2.Body.String() != "complete response data" {
			t.Errorf("BUG: partial or incorrect response was cached! Got: %s", rw2.Body.String())
		}

		done <- true
	}()

	// Wait for completion with timeout
	select {
	case <-done:
		t.Log("Test completed successfully")
	case <-time.After(2 * time.Second):
		t.Fatal("BUG: Second request hung! The cache lock was never released after panic. This means finalize() was never called.")
	}

	// Verify no partial or temp files remain in the cache directory
	verifyNoTempFiles(t, dir)
}

func TestCache_NoCacheControl(t *testing.T) {
	dir := createTempDir(t)

	next := func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
	}

	cfg := &Config{Path: dir, MaxExpiry: 10, Cleanup: 20, AddStatusHeader: true, MaxHeaderPairs: 2, MaxHeaderKeyLen: 30, MaxHeaderValueLen: 100}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/some/path", nil)
	rw := httptest.NewRecorder()

	c.ServeHTTP(rw, req)

	if state := rw.Header().Get("Cache-Status"); state != cacheMissStatus {
		t.Errorf("unexpected cache state: want \"miss\", got: %q", state)
	}

	rw = httptest.NewRecorder()

	c.ServeHTTP(rw, req)

	if state := rw.Header().Get("Cache-Status"); state != cacheHitStatus {
		t.Errorf("unexpected cache state: want \"hit\", got: %q", state)
	}
}

// failingResponseWriter simulates a downstream client that fails after writing N bytes
type failingResponseWriter struct {
	http.ResponseWriter
	written        int
	failAfterBytes int
}

func (w *failingResponseWriter) Write(p []byte) (int, error) {
	if w.written+len(p) > w.failAfterBytes {
		// Fail partway through - return error like a real disconnect would
		n := w.failAfterBytes - w.written
		if n > 0 {
			w.written += n
			_, _ = w.ResponseWriter.Write(p[:n])
		}
		return n, errors.New("downstream connection failed")
	}

	n, err := w.ResponseWriter.Write(p)
	w.written += n
	return n, err
}

func createTempDir(tb testing.TB) string {
	tb.Helper()

	dir, err := ioutil.TempDir("./", "example")
	if err != nil {
		tb.Fatal(err)
	}

	tb.Cleanup(func() {
		if err = os.RemoveAll(dir); err != nil {
			tb.Fatal(err)
		}
	})

	return dir
}

func TestCache_DoubleCheckedLocking(t *testing.T) {
	dir := createTempDir(t)

	// Track upstream requests
	var upstreamCalls int32
	requestDelay := 100 * time.Millisecond

	next := func(rw http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		// Simulate slow upstream
		time.Sleep(requestDelay)
		rw.Header().Set("Cache-Control", "max-age=20")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("response data"))
	}

	cfg := &Config{
		Path:              dir,
		MaxExpiry:         10,
		Cleanup:           20,
		AddStatusHeader:   true,
		MaxHeaderPairs:    2,
		MaxHeaderKeyLen:   30,
		MaxHeaderValueLen: 100,
		UpdateTimeout:     30, // 30 second timeout for waiting
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	// Launch 10 concurrent requests for the same resource
	const numRequests = 10
	var wg sync.WaitGroup
	results := make([]string, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "http://localhost/test/path", nil)
			rw := httptest.NewRecorder()
			c.ServeHTTP(rw, req)
			results[idx] = rw.Header().Get("Cache-Status")
		}(i)
	}

	wg.Wait()

	// Check results
	upstreamCount := atomic.LoadInt32(&upstreamCalls)
	t.Logf("Upstream was called %d times for %d concurrent requests", upstreamCount, numRequests)

	// With double-checked locking, we should have:
	// - 1 upstream call (first request)
	// - Remaining requests either wait and get cache hit, or also call upstream
	// The ideal is 1, but we might get 2-3 due to timing
	if upstreamCount > 3 {
		t.Errorf("Too many upstream calls: got %d, expected <= 3 (double-checked locking not working)", upstreamCount)
	}

	// Count cache hits vs misses
	hits := 0
	misses := 0
	for _, status := range results {
		if status == "hit" {
			hits++
		} else if status == "miss" {
			misses++
		}
	}

	t.Logf("Results: %d hits, %d misses", hits, misses)

	// We should have at least some cache hits
	if hits == 0 {
		t.Error("Expected at least some cache hits from double-checked locking")
	}
}

func TestCache_UpdateTimeout(t *testing.T) {
	dir := createTempDir(t)

	// Track upstream requests
	var upstreamCalls int32

	next := func(rw http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		// Simulate VERY slow upstream (hangs for 60 seconds)
		time.Sleep(60 * time.Second)
		rw.Header().Set("Cache-Control", "max-age=20")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("response data"))
	}

	cfg := &Config{
		Path:              dir,
		MaxExpiry:         10,
		Cleanup:           20,
		AddStatusHeader:   true,
		MaxHeaderPairs:    2,
		MaxHeaderKeyLen:   30,
		MaxHeaderValueLen: 100,
		UpdateTimeout:     1, // 1 second timeout - should fire
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	// Launch 2 requests concurrently
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "http://localhost/slow", nil)
			rw := httptest.NewRecorder()
			c.ServeHTTP(rw, req)
		}()
	}

	// Wait with overall timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		elapsed := time.Since(start)
		upstreamCount := atomic.LoadInt32(&upstreamCalls)

		t.Logf("Both requests completed in %v", elapsed)
		t.Logf("Upstream called %d times", upstreamCount)

		// Should have 2 upstream calls (timeout causes second request to fetch too)
		if upstreamCount != 2 {
			t.Errorf("Expected 2 upstream calls due to timeout, got %d", upstreamCount)
		}

		// Should complete in ~60s (both running in parallel after timeout)
		// not 120s (sequential)
		if elapsed > 70*time.Second {
			t.Errorf("Took too long: %v (timeout not working?)", elapsed)
		}

	case <-time.After(75 * time.Second):
		t.Fatal("Test timed out - requests hung")
	}
}

func TestCache_StartupCleansUpTempFiles(t *testing.T) {
	dir := createTempDir(t)

	// Create some fake temp files to simulate crash leftovers
	tempFile1 := filepath.Join(dir, "fakehash.tmp.1234567890abcdef")
	tempFile2 := filepath.Join(dir, "anotherhash.tmp.fedcba0987654321")

	if err := os.WriteFile(tempFile1, []byte("orphaned temp data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tempFile2, []byte("more orphaned data"), 0600); err != nil {
		t.Fatal(err)
	}

	// Verify temp files exist before cache creation
	if _, err := os.Stat(tempFile1); os.IsNotExist(err) {
		t.Fatal("Test setup failed: temp file 1 was not created")
	}
	if _, err := os.Stat(tempFile2); os.IsNotExist(err) {
		t.Fatal("Test setup failed: temp file 2 was not created")
	}

	// Create a cache instance - this should clean up temp files on startup
	cfg := &Config{
		Path:              dir,
		MaxExpiry:         10,
		Cleanup:           20,
		AddStatusHeader:   true,
		MaxHeaderPairs:    2,
		MaxHeaderKeyLen:   30,
		MaxHeaderValueLen: 100,
	}

	next := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	_, err := New(context.Background(), next, cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	// Verify temp files were cleaned up
	verifyNoTempFiles(t, dir)
}

func TestCache_EmptyBodyCached(t *testing.T) {
	dir := createTempDir(t)

	// Upstream sends cacheable headers but no body — this is a legitimate empty response
	next := func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Cache-Control", "max-age=20")
		rw.WriteHeader(http.StatusOK)
		// No rw.Write() call — 0-byte body, but request completed normally
	}

	cfg := &Config{
		Path:              dir,
		MaxExpiry:         10,
		Cleanup:           20,
		AddStatusHeader:   true,
		MaxHeaderPairs:    2,
		MaxHeaderKeyLen:   30,
		MaxHeaderValueLen: 100,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://localhost/empty", nil)
	rw := httptest.NewRecorder()

	c.ServeHTTP(rw, req)

	if state := rw.Header().Get("Cache-Status"); state != "miss" {
		t.Errorf("first request: expected cache miss, got %q", state)
	}

	// Second request should be a hit — a legitimate empty response with cache headers should be cached
	rw2 := httptest.NewRecorder()
	c.ServeHTTP(rw2, req)

	if state := rw2.Header().Get("Cache-Status"); state != "hit" {
		t.Errorf("second request: expected cache hit (legitimate empty body should be cached), got %q", state)
	}

	verifyNoTempFiles(t, dir)
}

func TestCache_ContextCancelledNotCached(t *testing.T) {
	dir := createTempDir(t)

	var calls int32

	// First call: send cacheable headers, then block until context is cancelled (simulating timeout)
	// Second call: respond normally with a body
	next := func(rw http.ResponseWriter, req *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		rw.Header().Set("Cache-Control", "max-age=20")
		rw.WriteHeader(http.StatusOK)

		if n == 1 {
			// First request: context gets cancelled before body is written
			<-req.Context().Done()
			return
		}

		// Subsequent requests: respond normally
		_, _ = rw.Write([]byte("real response"))
	}

	cfg := &Config{
		Path:              dir,
		MaxExpiry:         10,
		Cleanup:           20,
		AddStatusHeader:   true,
		MaxHeaderPairs:    2,
		MaxHeaderKeyLen:   30,
		MaxHeaderValueLen: 100,
	}

	c, err := New(context.Background(), http.HandlerFunc(next), cfg, "cacheify")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "http://localhost/timeout", nil).WithContext(ctx)
	rw := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		c.ServeHTTP(rw, req)
		close(done)
	}()

	// Give the handler time to send headers, then cancel the context
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeHTTP hung after context cancellation")
	}

	// Second request with a fresh context — should be a miss, not a hit on the empty cached response
	req2 := httptest.NewRequest(http.MethodGet, "http://localhost/timeout", nil)
	rw2 := httptest.NewRecorder()
	c.ServeHTTP(rw2, req2)

	if state := rw2.Header().Get("Cache-Status"); state == "hit" {
		t.Errorf("second request: got cache hit after context cancellation — 0-byte response was incorrectly cached")
	}

	verifyNoTempFiles(t, dir)
}

// verifyNoTempFiles checks that no .tmp.* files remain in the cache directory
// This ensures partial writes are properly cleaned up
func verifyNoTempFiles(t *testing.T, dir string) {
	t.Helper()

	var tempFiles []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.Contains(filepath.Base(path), ".tmp.") {
			tempFiles = append(tempFiles, path)
		}
		return nil
	})

	if err != nil {
		t.Fatalf("Error walking cache directory: %v", err)
	}

	if len(tempFiles) > 0 {
		t.Errorf("BUG: Found %d temporary files that were not cleaned up:", len(tempFiles))
		for _, f := range tempFiles {
			t.Errorf("  - %s", f)
		}
		t.Fatal("Temporary files should be cleaned up after abort")
	}
}
