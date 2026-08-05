// Package cacheify is a plugin to cache responses to disk.
package cacheify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/pquerna/cachecontrol"
)

// Config configures the middleware.
type Config struct {
	Path                   string `json:"path" yaml:"path" toml:"path"`
	MaxExpiry              int    `json:"maxExpiry" yaml:"maxExpiry" toml:"maxExpiry"`
	Cleanup                int    `json:"cleanup" yaml:"cleanup" toml:"cleanup"`
	AddStatusHeader        bool   `json:"addStatusHeader" yaml:"addStatusHeader" toml:"addStatusHeader"`
	QueryInKey             bool   `json:"queryInKey" yaml:"queryInKey" toml:"queryInKey"`
	StripResponseCookies   bool   `json:"stripResponseCookies" yaml:"stripResponseCookies" toml:"stripResponseCookies"`
	NoCacheOnSetCookie     bool   `json:"noCacheOnSetCookie" yaml:"noCacheOnSetCookie" toml:"noCacheOnSetCookie"`
	NoCacheOnAuthorization bool   `json:"noCacheOnAuthorization" yaml:"noCacheOnAuthorization" toml:"noCacheOnAuthorization"`
	NoHeuristicCaching     bool   `json:"noHeuristicCaching" yaml:"noHeuristicCaching" toml:"noHeuristicCaching"`
	MaxHeaderPairs         int    `json:"maxHeaderPairs" yaml:"maxHeaderPairs" toml:"maxHeaderPairs"`
	MaxHeaderKeyLen        int    `json:"maxHeaderKeyLen" yaml:"maxHeaderKeyLen" toml:"maxHeaderKeyLen"`
	MaxHeaderValueLen      int    `json:"maxHeaderValueLen" yaml:"maxHeaderValueLen" toml:"maxHeaderValueLen"`
	UpdateTimeout          int    `json:"updateTimeout" yaml:"updateTimeout" toml:"updateTimeout"` // Seconds to wait for another request to complete cache update
}

// CreateConfig returns a config instance.
func CreateConfig() *Config {
	return &Config{
		MaxExpiry:              int((5 * time.Minute).Seconds()),
		Cleanup:                int((10 * time.Minute).Seconds()),
		AddStatusHeader:        true,
		QueryInKey:             true,
		StripResponseCookies:   true,
		NoCacheOnSetCookie:     true,
		NoCacheOnAuthorization: true,
		NoHeuristicCaching:     true,
		MaxHeaderPairs:         255,
		MaxHeaderKeyLen:        100,
		MaxHeaderValueLen:      8192,
		UpdateTimeout:          30, // 30 seconds default timeout waiting for cache updates
	}
}

const (
	cacheHeader      = "Cache-Status"
	cacheHitStatus   = "hit"
	cacheMissStatus  = "miss"
	cacheErrorStatus = "error"
)

type cache struct {
	name  string
	cache *fileCache
	cfg   *Config
	next  http.Handler
}

// Limits imposed by the on-disk metadata wire format (see marshalMetadata):
// a 2-byte pair count, a 2-byte key length and a 3-byte value length.
// Configuring a limit above these silently truncates on write, corrupting
// the cache file, so we reject such configs at startup instead.
const (
	maxWireHeaderPairs    = 1<<16 - 1
	maxWireHeaderKeyLen   = 1<<16 - 1
	maxWireHeaderValueLen = 1<<24 - 1
)

// New returns a plugin instance.
func New(_ context.Context, next http.Handler, cfg *Config, name string) (http.Handler, error) {
	if cfg.MaxExpiry <= 1 {
		return nil, errors.New("maxExpiry must be greater or equal to 1")
	}

	if cfg.Cleanup <= 1 {
		return nil, errors.New("cleanup must be greater or equal to 1")
	}

	if cfg.MaxHeaderPairs > maxWireHeaderPairs {
		return nil, fmt.Errorf("maxHeaderPairs must not exceed %d (on-disk format limit)", maxWireHeaderPairs)
	}

	if cfg.MaxHeaderKeyLen > maxWireHeaderKeyLen {
		return nil, fmt.Errorf("maxHeaderKeyLen must not exceed %d (on-disk format limit)", maxWireHeaderKeyLen)
	}

	if cfg.MaxHeaderValueLen > maxWireHeaderValueLen {
		return nil, fmt.Errorf("maxHeaderValueLen must not exceed %d (on-disk format limit)", maxWireHeaderValueLen)
	}

	fc, err := newFileCache(
		cfg.Path,
		time.Duration(cfg.Cleanup)*time.Second,
		cfg.MaxHeaderPairs,
		cfg.MaxHeaderKeyLen,
		cfg.MaxHeaderValueLen,
	)
	if err != nil {
		return nil, err
	}

	m := &cache{
		name:  name,
		cache: fc,
		cfg:   cfg,
		next:  next,
	}

	return m, nil
}

type cacheData struct {
	Status  int
	Headers map[string][]string
	Body    []byte
}

// ServeHTTP serves an HTTP request.
func (m *cache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Bypass cache for protocol upgrade requests (e.g. WebSocket).
	// These are long-lived bidirectional connections that cannot be cached,
	// and the upgrade requires http.Hijacker support on the ResponseWriter
	// which our responseWriter wrapper does not expose.
	if r.Header.Get("Upgrade") != "" {
		m.next.ServeHTTP(w, r)
		return
	}

	// Bypass the cache entirely (both read AND write) for Range requests.
	// This plugin has no support for serving byte-range slices out of a
	// cached entry, nor for storing per-range variants of a resource. Without
	// this, a Range request could populate the cache with a 206 Partial
	// Content body that a later, full request would then receive verbatim
	// (truncated/wrong content, served as if it were the complete resource),
	// or an existing full 200 entry could be served back to a Range request
	// while completely ignoring the requested range.
	if r.Header.Get("Range") != "" {
		if m.cfg.AddStatusHeader {
			w.Header().Set(cacheHeader, cacheMissStatus)
		}
		m.next.ServeHTTP(w, r)
		return
	}

	// Bypass the cache entirely (both read AND write) for Authorization-bearing
	// requests, matching Varnish's default vcl_recv "return (pass)" behaviour.
	// This must happen before any cache lookup: a check inside cacheable()
	// alone only stops an authenticated response from being *written* - an
	// authenticated request could still receive a *hit* that an earlier,
	// unauthenticated request for the same URL had already cached, since the
	// cache key does not incorporate Authorization. That would silently serve
	// the wrong (generic/anonymous) content to an authenticated caller. Doing
	// the bypass here also avoids needlessly serialising concurrent
	// Authorization-bearing requests through the update-intent machinery,
	// since none of them will ever be cached anyway.
	if m.cfg.NoCacheOnAuthorization && r.Header.Get("Authorization") != "" {
		if m.cfg.AddStatusHeader {
			w.Header().Set(cacheHeader, cacheMissStatus)
		}
		m.next.ServeHTTP(w, r)
		return
	}

	cs := cacheMissStatus

	key := cacheKey(r, m.cfg.QueryInKey)

	// First check: Try to serve from cache (non-blocking read)
	cached, err := m.cache.GetStream(key)
	if err == nil {
		defer cached.Body.Close()

		// Write headers
		for key, vals := range cached.Metadata.Headers {
			for _, val := range vals {
				w.Header().Add(key, val)
			}
		}
		if m.cfg.AddStatusHeader {
			w.Header().Set(cacheHeader, cacheHitStatus)
		}

		// Write status
		w.WriteHeader(cached.Metadata.Status)

		// Stream body using pooled buffer to reduce allocations
		buf := copyBufferPool.Get().(*[]byte)
		_, _ = io.CopyBuffer(w, cached.Body, *buf)
		copyBufferPool.Put(buf)
		return
	}

	// Cache miss - use double-checked locking via update intent
	// Try to claim responsibility for fetching this resource
	claimed := m.cache.claimUpdateIntent(key)
	if !claimed {
		// Someone else claimed it - wait for them to finish (with timeout)
		timeout := time.Duration(m.cfg.UpdateTimeout) * time.Second
		completed := m.cache.waitForUpdateIntent(key, timeout)

		if completed {
			// Wait completed successfully - try cache again now that they're done
			cached, err := m.cache.GetStream(key)
			if err == nil {
				defer cached.Body.Close()

				// Write headers
				for key, vals := range cached.Metadata.Headers {
					for _, val := range vals {
						w.Header().Add(key, val)
					}
				}
				if m.cfg.AddStatusHeader {
					w.Header().Set(cacheHeader, cacheHitStatus)
				}

				// Write status
				w.WriteHeader(cached.Metadata.Status)

				// Stream body using pooled buffer to reduce allocations
				buf := copyBufferPool.Get().(*[]byte)
				_, _ = io.CopyBuffer(w, cached.Body, *buf)
				copyBufferPool.Put(buf)
				return
			}
		} else {
			// Timeout waiting - other request may be hung/slow
			// Fall through to fetch ourselves, but first try to claim the intent
			log.Printf("Timeout waiting for cache update, proceeding with upstream fetch")
			claimed = m.cache.claimUpdateIntent(key)
		}
		// If timeout or still a miss, fall through to fetch ourselves
	}

	// Only release the update intent if we actually claimed it
	if claimed {
		defer m.cache.releaseUpdateIntent(key)
	}

	// Cache miss - proceed with backend request
	// Set cache status header before backend call so it's included in response
	if m.cfg.AddStatusHeader {
		w.Header().Set(cacheHeader, cs)
	}

	rw := &responseWriter{
		ResponseWriter: w,
		cache:          m.cache,
		cacheKey:       key,
		request:        r,
		config:         m.cfg,
		checkCacheable: m.cacheable,
	}

	// Ensure finalize is called to commit or abort cache write
	// If upstream panics, mark writeErr so finalize() aborts instead of commits
	defer func() {
		if r := recover(); r != nil {
			// Upstream handler panicked - ensure we abort the cache write
			rw.writeErr = errors.New("upstream handler panicked")
			// Let the request fail gracefully (don't re-panic)
			log.Printf("Upstream handler panic (aborting cache write): %v", r)
		}

		// Always finalize (commit if no errors, abort if writeErr is set)
		if err := rw.finalize(); err != nil {
			log.Printf("Error finalizing cache: %v", err)
		}
	}()

	m.next.ServeHTTP(rw, r)
}

// varySafelist lists request headers that are safe for us to ignore when
// deciding cacheability. Varying a response by these headers only affects
// presentation/encoding, never per-user/per-session content.
var varySafelist = map[string]bool{
	"accept-encoding": true,
	"accept-language": true,
}

// varyIsCacheable reports whether every header named in the response's Vary
// header(s) is on the safelist. Our cache key does not incorporate any
// request headers, so a Vary naming anything else (Cookie, Authorization,
// Accept, ...) - or "Vary: *" - means the response legitimately differs
// per client/session in a way we cannot key on. Caching it would mean
// serving one user's variant to everyone else matching the same URL.
func varyIsCacheable(w http.ResponseWriter) bool {
	for _, vary := range w.Header().Values("Vary") {
		for _, field := range strings.Split(vary, ",") {
			field = strings.ToLower(strings.TrimSpace(field))
			if field == "" {
				continue
			}
			if field == "*" || !varySafelist[field] {
				return false
			}
		}
	}
	return true
}

func (m *cache) cacheable(r *http.Request, w http.ResponseWriter, status int) (time.Duration, bool) {
	// This plugin always replays a cached entry's stored status/body verbatim
	// to every subsequent request, regardless of that request's own
	// Range/If-None-Match/If-Modified-Since headers. The underlying
	// cachecontrol library treats both of these as cacheable by default
	// (206 is explicitly RFC-listed as heuristically cacheable; 304 becomes
	// cacheable whenever the origin also sends an explicit Cache-Control,
	// which is a common, spec-compliant pattern for revalidation responses).
	// Caching either would let one client's Range or conditional request
	// poison the cache for every other client requesting the same URL: a
	// 206 stores only a byte-slice that a later full request would then
	// receive as if it were the complete body, and a 304 stores an empty
	// body that a later request with no conditional headers would receive
	// as if it were the full resource. Block both unconditionally - a
	// Range request is also bypassed entirely in ServeHTTP before this is
	// ever reached, but 304 has no equivalent request-side signal to bypass
	// on, so it must be caught here instead.
	if status == http.StatusPartialContent || status == http.StatusNotModified {
		return 0, false
	}

	// Varnish-style behaviour: a Set-Cookie header almost always means a
	// per-client/per-session response. Treat its mere presence as an
	// unconditional "do not cache", regardless of what Cache-Control says,
	// since origins frequently forget to mark such responses private/no-store.
	if m.cfg.NoCacheOnSetCookie && len(w.Header().Values("Set-Cookie")) > 0 {
		return 0, false
	}

	// NoCacheOnAuthorization is enforced as a full read+write bypass in
	// ServeHTTP, before a cache lookup even happens - see the comment there.
	// By the time cacheable() runs (only reachable on a cache miss), an
	// Authorization-bearing request has therefore already been routed
	// straight to the backend when the option is enabled, so no check is
	// needed here. When the option is disabled, RFC 7234 §3.2's
	// Authorization-request handling in the cachecontrol library below
	// still applies (Cache-Control: public/must-revalidate/s-maxage
	// re-enables caching, matching Varnish's default).

	if !varyIsCacheable(w) {
		return 0, false
	}

	reasons, expireBy, err := cachecontrol.CachableResponseWriter(r, status, w, cachecontrol.Options{})
	if err != nil || len(reasons) > 0 {
		return 0, false
	}

	if expireBy.IsZero() {
		// No explicit expiration signal (no Cache-Control max-age/s-maxage,
		// no Public directive, no Expires header) - the response only ended
		// up here because its status code is heuristically cacheable by
		// default per RFC 7234 §4.2.2. That default exists for caches that
		// implement heuristic freshness properly (e.g. deriving a lifetime
		// from Last-Modified); this plugin instead just applies the flat
		// MaxExpiry ceiling to anything, including responses whose origin
		// never made any caching decision at all - i.e. ordinary dynamic
		// endpoints that simply forgot to set Cache-Control: no-store. With
		// NoHeuristicCaching enabled, require an explicit signal instead of
		// guessing.
		if m.cfg.NoHeuristicCaching {
			return 0, false
		}

		return time.Duration(m.cfg.MaxExpiry) * time.Second, true
	}

	expiry := time.Until(expireBy)
	maxExpiry := time.Duration(m.cfg.MaxExpiry) * time.Second

	if maxExpiry < expiry {
		expiry = maxExpiry
	}

	return expiry, true
}

func cacheKey(r *http.Request, includeQuery bool) string {
	// Use strings.Builder to avoid multiple allocations
	var b strings.Builder

	// Pre-allocate approximate capacity
	b.Grow(len(r.Method) + len(r.Host) + len(r.URL.Path) + len(r.URL.RawQuery) + 10)

	// Base key with method, host and path. NUL-separated so that e.g. a
	// method "GETX" + host "ample.com" can never collide with method "GET"
	// + host "Xample.com" once concatenated.
	b.WriteString(r.Method)
	b.WriteByte(0)
	b.WriteString(strings.ToLower(r.Host))
	b.WriteByte(0)
	b.WriteString(r.URL.Path)

	// Handle query parameters in a sorted, consistent way
	if includeQuery && r.URL.RawQuery != "" {
		query := r.URL.Query() // Parse once and cache

		if len(query) > 0 {
			// Get all query parameter keys
			params := make([]string, 0, len(query))
			for param := range query {
				params = append(params, param)
			}

			// Sort the parameter keys
			sort.Strings(params)

			b.WriteByte('?')
			first := true
			for _, param := range params {
				values := query[param]
				sort.Strings(values)

				for _, value := range values {
					if !first {
						b.WriteByte('&')
					}
					first = false
					b.WriteString(url.QueryEscape(param))
					b.WriteByte('=')
					b.WriteString(url.QueryEscape(value))
				}
			}
		}
	}

	return b.String()
}

type responseWriter struct {
	http.ResponseWriter
	cache          *fileCache
	cacheKey       string
	request        *http.Request
	config         *Config
	checkCacheable func(*http.Request, http.ResponseWriter, int) (time.Duration, bool)

	status        int
	headerWritten bool
	wasCached     bool
	cacheWriter   *streamingCacheWriter
	writeErr      error // Track if any write errors occurred
}

func (rw *responseWriter) Header() http.Header {
	return rw.ResponseWriter.Header()
}

func (rw *responseWriter) WriteHeader(s int) {
	if rw.headerWritten {
		return
	}
	rw.headerWritten = true
	rw.status = s

	// Make cache decision now that we have status and headers
	expiry, cacheable := rw.checkCacheable(rw.request, rw.ResponseWriter, s)

	if cacheable {
		// Strip Set-Cookie headers if configured (affects both cache and response).
		// With the default NoCacheOnSetCookie=true, responses carrying Set-Cookie
		// never reach this branch at all, so this mainly matters for deployments
		// that explicitly disable NoCacheOnSetCookie.
		if rw.config.StripResponseCookies {
			rw.ResponseWriter.Header().Del("Set-Cookie")
		}

		// Try to start streaming cache write (non-blocking for double-checked locking)
		metadata := cacheMetadata{
			Status:  s,
			Headers: rw.ResponseWriter.Header(),
		}

		var err error
		rw.cacheWriter, err = rw.cache.SetStream(rw.cacheKey, metadata, expiry)
		if err != nil {
			// errCacheWriteInProgress means another request beat us to it
			// That's fine - they'll populate the cache, we just stream from upstream
			if !errors.Is(err, errCacheWriteInProgress) {
				log.Printf("Error starting cache write: %v", err)
			}
		} else {
			rw.wasCached = true
		}
	}

	rw.ResponseWriter.WriteHeader(s)
}

func (rw *responseWriter) Write(p []byte) (int, error) {
	// Ensure WriteHeader was called
	if !rw.headerWritten {
		rw.WriteHeader(http.StatusOK)
	}

	// Write to cache if we're caching
	if rw.cacheWriter != nil {
		if _, err := rw.cacheWriter.Write(p); err != nil {
			log.Printf("Error writing to cache: %v", err)
			// Don't fail the request, just stop caching
			_ = rw.cacheWriter.Abort()
			rw.cacheWriter = nil
			rw.writeErr = err
		}
	}

	// Always write to client
	n, err := rw.ResponseWriter.Write(p)
	if err != nil && rw.writeErr == nil {
		rw.writeErr = err
	}
	return n, err
}

func (rw *responseWriter) finalize() error {
	if rw.cacheWriter == nil {
		return nil
	}

	// Abort if the response may be incomplete
	var abortReason string
	if rw.writeErr != nil {
		abortReason = fmt.Sprintf("write error: %v", rw.writeErr)
	} else if err := rw.request.Context().Err(); err != nil {
		abortReason = fmt.Sprintf("request context cancelled: %v", err)
	}

	if abortReason != "" {
		log.Printf("Aborting cache write for %s: %s", rw.cacheKey, abortReason)
		return rw.cacheWriter.Abort()
	}

	return rw.cacheWriter.Commit()
}
