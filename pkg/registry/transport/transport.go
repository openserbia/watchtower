// Package transport builds the http.Client used for registry API calls.
//
// It replaces the old blanket `InsecureSkipVerify: true` in pkg/registry/digest
// with opt-in per-host insecurity: TLS verification is enforced by default, and
// the operator must list hosts explicitly via --insecure-registry to skip
// verification (or provide a --registry-ca-bundle with custom trusted roots).
package transport

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	dialerTimeout         = 30 * time.Second
	dialerKeepAlive       = 30 * time.Second
	idleConnMax           = 100
	idleConnTimeout       = 90 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	expectContinueTimeout = 1 * time.Second
)

// Config captures the operator-provided TLS tuning for registry HTTP calls.
type Config struct {
	// InsecureRegistries is a set of hostnames ("example.com" or "host:port")
	// for which TLS certificate verification is skipped. Matches the host
	// component of the request URL verbatim — no wildcard expansion.
	InsecureRegistries map[string]struct{}
	// RootCAs, if non-nil, is the pool used to verify server certs for all
	// registries NOT in InsecureRegistries. When nil, the system cert pool
	// is used (http's default behavior).
	RootCAs *x509.CertPool
}

var (
	mu        sync.RWMutex
	cfg       = &Config{InsecureRegistries: map[string]struct{}{}}
	secure    *http.Transport
	permitted *http.Transport
)

func init() {
	rebuildTransports()
}

// Configure replaces the active transport configuration. Safe to call from
// startup (cmd/root.go PreRun) — the constructed transports are immutable and
// handed out lazily to each caller of Client.
func Configure(insecureRegistries []string, caBundlePath string) error {
	newCfg := &Config{
		InsecureRegistries: make(map[string]struct{}, len(insecureRegistries)),
	}
	for _, host := range insecureRegistries {
		if host != "" {
			newCfg.InsecureRegistries[host] = struct{}{}
		}
	}

	if caBundlePath != "" {
		pem, err := os.ReadFile(caBundlePath)
		if err != nil {
			return fmt.Errorf("read CA bundle %q: %w", caBundlePath, err)
		}
		// Start from the system pool so operator-provided roots *extend* the
		// default trust store rather than replacing it.
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return errors.New("CA bundle contains no valid PEM certificates")
		}
		newCfg.RootCAs = pool
	}

	mu.Lock()
	cfg = newCfg
	rebuildTransports()
	mu.Unlock()
	return nil
}

// Client returns an http.Client wired with the right TLS policy for the given
// host (the URL's host:port). Callers pass the host they're about to call so
// the per-host insecure override works without sniffing every request.
func Client(host string) *http.Client {
	mu.RLock()
	defer mu.RUnlock()
	if _, insecure := cfg.InsecureRegistries[host]; insecure {
		return &http.Client{Transport: permitted}
	}
	return &http.Client{Transport: secure}
}

// IsInsecure reports whether the operator opted this host out of TLS verification.
func IsInsecure(host string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := cfg.InsecureRegistries[host]
	return ok
}

func rebuildTransports() {
	base := func() *http.Transport {
		return &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   dialerTimeout,
				KeepAlive: dialerKeepAlive,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          idleConnMax,
			IdleConnTimeout:       idleConnTimeout,
			TLSHandshakeTimeout:   tlsHandshakeTimeout,
			ExpectContinueTimeout: expectContinueTimeout,
		}
	}

	secure = base()
	secure.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    cfg.RootCAs,
	}

	permitted = base()
	permitted.TLSClientConfig = &tls.Config{
		//nolint:gosec // Opt-in per host via --insecure-registry. See package doc.
		InsecureSkipVerify: true,
	}
}
