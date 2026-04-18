package auth

import (
	"testing"
	"time"
)

func TestCacheKeyStableForSameInputs(t *testing.T) {
	t.Parallel()

	k1 := cacheKey("https://ghcr.io/token?scope=repository:foo/bar:pull&service=ghcr.io", "user:pass")
	k2 := cacheKey("https://ghcr.io/token?scope=repository:foo/bar:pull&service=ghcr.io", "user:pass")
	if k1 != k2 {
		t.Fatalf("cacheKey should be stable: got %q vs %q", k1, k2)
	}
}

func TestCacheKeyDiffersByCredential(t *testing.T) {
	t.Parallel()

	authURL := "https://ghcr.io/token?scope=repository:foo/bar:pull&service=ghcr.io"
	if cacheKey(authURL, "alice:pw") == cacheKey(authURL, "bob:pw") {
		t.Fatal("cacheKey should differ when credentials differ")
	}
	if cacheKey(authURL, "anon") == cacheKey(authURL, "") {
		t.Fatal("cacheKey should differ between anonymous and authenticated")
	}
}

func TestCacheGetMissReturnsFalse(t *testing.T) {
	t.Parallel()

	c := newTokenCache()
	if _, ok := c.get("missing"); ok {
		t.Fatal("empty cache should miss")
	}
}

func TestCacheSetAndGetReturnsValue(t *testing.T) {
	t.Parallel()

	c := newTokenCache()
	c.set("k", "Bearer xyz", time.Hour)

	header, ok := c.get("k")
	if !ok {
		t.Fatal("expected cache hit after set")
	}
	if header != "Bearer xyz" {
		t.Fatalf("unexpected header: %q", header)
	}
}

func TestCacheGetExpiredEntryReturnsFalse(t *testing.T) {
	t.Parallel()

	c := newTokenCache()
	c.entries["k"] = cachedToken{
		header:    "Bearer stale",
		expiresAt: time.Now().Add(-time.Second),
	}

	if _, ok := c.get("k"); ok {
		t.Fatal("expired entry should miss")
	}
}

func TestCacheSetSkipsWhenTTLAtOrBelowSkew(t *testing.T) {
	t.Parallel()

	c := newTokenCache()
	c.set("k", "Bearer short", tokenExpirySkew)
	if _, ok := c.get("k"); ok {
		t.Fatal("TTL <= skew should not cache")
	}

	c.set("k", "Bearer shorter", tokenExpirySkew-time.Second)
	if _, ok := c.get("k"); ok {
		t.Fatal("TTL < skew should not cache")
	}
}

func TestCacheSetAppliesSkew(t *testing.T) {
	t.Parallel()

	c := newTokenCache()
	c.set("k", "Bearer ok", time.Minute)

	entry, ok := c.entries["k"]
	if !ok {
		t.Fatal("expected entry to be stored")
	}

	remaining := time.Until(entry.expiresAt)
	// Effective TTL should be (ttl - skew) = 50s. Allow a small wall-clock margin.
	if remaining > time.Minute-tokenExpirySkew+time.Second || remaining < time.Minute-tokenExpirySkew-time.Second {
		t.Fatalf("unexpected expiry: remaining=%s, want ~%s", remaining, time.Minute-tokenExpirySkew)
	}
}
