**DO NOTE:** This fork exists because I wanted to mess about with AI coding. This should not be used, do not rely on my changes. My changes are slop!!

# Cacheify

Simple cache plugin middleware caches responses on disk.

Based on the original plugin-simplecache, but with some significant performance improvements
- Cache hits are now up to 13× faster and use 45% less memory, with large payloads seeing over 90% latency reduction.
- Streamlined miss handling and buffer reuse (-45–60% memory use on large bodies)
- Reduced allocations per operation by ~20% overall
- Improved concurrent hit performance (no longer contended)
- Simplified and accelerated cache key generation (-75–88% time)
- Achieved ~36% faster benchmarks overall and ~60% higher throughput

## Platform Support

**Unix/Linux/macOS only** - This plugin is not compatible with Windows due to Traefik's Yaegi interpreter security restrictions on `unsafe` and `syscall` packages required for atomic file operations on Windows.

## Configuration

To configure this plugin you should add its configuration to the Traefik dynamic configuration as explained [here](https://docs.traefik.io/getting-started/configuration-overview/#the-dynamic-configuration).
The following snippet shows how to configure this plugin with the File provider in TOML and YAML: 

Static:

```toml
[experimental.plugins.cache]
  modulename = "github.com/sevvlor/cacheify"
  version = "v0.0.1"
```

Dynamic:

```toml
[http.middlewares]
  [http.middlewares.my-cache.plugin.cache]
    path = "/some/path/to/cache/dir"
```

```yaml
http:
  middlewares:
   my-cache:
      plugin:
        cache:
          path: /some/path/to/cache/dir
```

### Options

#### Path (`path`)

The base path that files will be created under. This must be a valid existing
filesystem path.

#### Max Expiry (`maxExpiry`)

*Default: 300*

The maximum number of seconds a response can be cached for. The 
actual cache time will always be lower or equal to this.

#### Cleanup (`cleanup`)

*Default: 600*

The number of seconds to wait between cache cleanup runs.
	
#### Add Status Header (`addStatusHeader`)

*Default: true*

This determines if the cache status header `Cache-Status` will be added to the
response headers. This header can have the value `hit`, `miss` or `error`.

#### Include Query Parameters in Cache Key (`queryInKey`)
*Default: true*

This determines whether the query parameters on the url form part of the key used for storing cacheable requests.

#### Max Header Pairs (`maxHeaderPairs`)
*Default: 255*

The maximum number of header key-value pairs allowed in cached responses. This prevents disk bloat attacks from responses with excessive headers. Multi-value headers (e.g., multiple `Set-Cookie` headers) count as separate pairs. Must not exceed 65535 (the on-disk format uses a 2-byte pair count); values above that are rejected at startup rather than silently truncated.

#### Max Header Key Length (`maxHeaderKeyLen`)
*Default: 100*

The maximum length in bytes for header keys (names). This prevents disk bloat from maliciously long header names. Standard HTTP header names are typically 10-30 bytes. Must not exceed 65535 (the on-disk format uses a 2-byte key length); values above that are rejected at startup rather than silently truncated.

#### Max Header Value Length (`maxHeaderValueLen`)
*Default: 8192*

The maximum length in bytes for header values. This prevents disk bloat from oversized cookies, tokens, or other header values. The default allows for large JWTs and session cookies while preventing abuse. Must not exceed 16777215 (the on-disk format uses a 3-byte value length); values above that are rejected at startup rather than silently truncated.

#### Strip Response Cookies (`stripResponseCookies`)
*Default: true*

If true (the default) cacheify will remove any 'Set-Cookie' headers from any cacheable responses (including the original request.) With the default `noCacheOnSetCookie` behaviour, responses carrying `Set-Cookie` are never cached in the first place, so this option mainly matters if you've disabled `noCacheOnSetCookie`.

#### No Cache On Set-Cookie (`noCacheOnSetCookie`)
*Default: true*

If true (the default), any response carrying a `Set-Cookie` header is treated as unconditionally non-cacheable, regardless of its `Cache-Control` headers - matching Varnish's default `hit_for_pass` behaviour for `Set-Cookie` responses. This exists because origins frequently forget to mark per-session/personalized responses as `private`/`no-store`, and a shared cache that only trusts `Cache-Control` can end up serving one user's cookie-bearing response (and its body) to another user. Disable this only if you are certain every route behind this middleware sets `Cache-Control` correctly for personalized content.

#### No Cache On Authorization (`noCacheOnAuthorization`)
*Default: true*

If true (the default), any request carrying an `Authorization` header is treated as unconditionally non-cacheable. RFC 7234 §3.2 (and the underlying `cachecontrol` library) normally allows an origin to override this by sending `Cache-Control: public`, `must-revalidate`, or `s-maxage` on the response - matching Varnish's default behaviour. That override is easy to trigger by accident: an origin that sets `Cache-Control: public` meaning simply "this may be cached" can unintentionally let one bearer token's response body be served to a request carrying a different (or no) token at the same URL. Disable this only if you deliberately rely on that override for a route that is genuinely token-agnostic (e.g. a public endpoint that merely accepts an optional `Authorization` header).

#### No Heuristic Caching (`noHeuristicCaching`)
*Default: true*

RFC 7234 §4.2.2 allows a cache to store a response with no explicit freshness signal at all (no `Cache-Control: max-age`/`s-maxage`/`public`, no `Expires`) for certain status codes, using a heuristic lifetime. This plugin's heuristic is simply "apply `maxExpiry`" - it does not distinguish between a static asset that forgot to set headers and an ordinary dynamic endpoint (e.g. a JSON API route) that never made a caching decision at all, because there was never a cache in front of it before. If true (the default), such headerless responses are never cached - only an explicit `Cache-Control`/`Expires` (or a `public` override) causes caching. Disable this only if you specifically want the pre-hardening behaviour of caching anything with a heuristically-cacheable status code by default.

#### Vary Handling

Cacheify's cache key does not incorporate any request headers, so a response whose `Vary` header names anything other than `Accept-Encoding` or `Accept-Language` (or is `Vary: *`) is treated as non-cacheable, regardless of `Cache-Control`. This prevents serving one client's `Vary: Cookie`/`Vary: Authorization` variant to every other client requesting the same URL. This is not currently configurable.

#### Update Timeout (`updateTimeout`)
*Default: 30*

The number of seconds to wait for another request to complete a cache update before timing out and fetching from upstream independently. This prevents requests from waiting indefinitely if an upstream server hangs during a cache miss. When multiple requests arrive for the same uncached resource, the first request fetches from upstream while subsequent requests wait for completion. If the timeout is exceeded, waiting requests will proceed to fetch from upstream themselves rather than block indefinitely.

## Upgrading onto an existing cache directory

Cacheability decisions (`noCacheOnSetCookie`, `noCacheOnAuthorization`, Vary handling) are only applied to *new* writes. There is no cache invalidation/purge mechanism in this plugin - entries already on disk from a previous version keep being served as cache hits until their own stored TTL expires, regardless of what the current code would have decided. If you are upgrading a deployment that was previously running without these protections, delete the contents of the configured `path` once as part of the rollout so no pre-hardening entries linger.

## Release History
### v1.0.0
#### Bugfixes
* Downstream (traefik) disconnects & timeouts could lead to 0 byte bodies being cached
* There was a memory leak identified in the lock management, the fix has incurred a minor performance penalty, but we are still largely IO bound.
#### Breaking change notice
This release introduces a potentially breaking change in behaviour. Prior to 1.0.0 there was a bug that meant responses that should be 'heuristically' cached, that is responses that don't explicitly decline caching behaviours but that should by default (according to the specification) be cached, were not being cached.

This release changes the behaviour so that these default 'heuristically' cacheable responses will now be cached, the heuristic used is currently whatever you have set in your maxExpiry setting.

This will likely result in a significantly higher number of responses being cached in v1.0.0 than previously and is not currently a configurable behaviour. If there is demand for that we can introduce it.
