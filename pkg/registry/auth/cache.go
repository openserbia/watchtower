package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// defaultTokenTTL is the token lifetime assumed when the registry omits
// `expires_in` from its token response, per the Docker registry token spec.
const defaultTokenTTL = 60 * time.Second

// tokenExpirySkew shortens cached TTLs so the cache never serves a token that
// is about to expire mid-request.
const tokenExpirySkew = 10 * time.Second

type cachedToken struct {
	header    string
	expiresAt time.Time
}

// tokenCache is a concurrency-safe map of auth-URL + credential → bearer header.
// Entries expire based on the registry-declared TTL minus a small skew.
type tokenCache struct {
	mu      sync.RWMutex
	entries map[string]cachedToken
}

func newTokenCache() *tokenCache {
	return &tokenCache{entries: make(map[string]cachedToken)}
}

// bearerCache is the process-wide cache used by GetBearerHeader.
var bearerCache = newTokenCache()

func cacheKey(authURL, registryAuth string) string {
	sum := sha256.Sum256([]byte(registryAuth))
	return authURL + "|" + hex.EncodeToString(sum[:])
}

func (c *tokenCache) get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return "", false
	}
	return entry.header, true
}

func (c *tokenCache) set(key, header string, ttl time.Duration) {
	if ttl <= tokenExpirySkew {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = cachedToken{
		header:    header,
		expiresAt: time.Now().Add(ttl - tokenExpirySkew),
	}
}
