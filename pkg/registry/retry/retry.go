// Package retry wraps registry HTTP requests in a bounded exponential backoff
// so a single flaky oauth/manifest response doesn't wedge an image for a full
// poll interval.
package retry

import (
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/pkg/metrics"
)

// Tunables are deliberately conservative: registry flakes usually recover
// within a second or two, and we do not want retries to bloat the poll cycle
// for legitimately-broken registries.
const (
	maxAttempts  = 3
	jitterFactor = 0.25
)

// Kept as vars so tests can shorten them without waiting out real backoff.
var (
	backoffBase = 500 * time.Millisecond
	backoffMax  = 4 * time.Second
)

// DoHTTP executes req via client, retrying on transient registry failures
// (network errors, 5xx, 429, and the 401/403/404 responses observed when
// registry oauth endpoints flake). The caller's req must be safe to replay —
// registry flows are all GET/HEAD, so that's the case everywhere this is used.
//
// operation is a short logical label ("challenge" / "token" / "digest") used
// for the watchtower_registry_requests_total metric. Pass "" to skip metric
// emission (useful in tests).
func DoHTTP(client *http.Client, req *http.Request, operation string, logger *logrus.Entry) (*http.Response, error) {
	host := req.URL.Host
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := backoffFor(attempt)
			if logger != nil {
				logger.Debugf("Retrying registry request after %s (attempt %d/%d)", delay, attempt+1, maxAttempts)
			}
			if operation != "" {
				metrics.RegisterRegistryRetry(host)
			}
			time.Sleep(delay)
		}

		res, err := client.Do(req)
		if err != nil {
			lastErr = err
			if operation != "" {
				metrics.RegisterRegistryRequest(host, operation, "error")
			}
			continue
		}

		if !isTransientStatus(res.StatusCode) {
			if operation != "" {
				metrics.RegisterRegistryRequest(host, operation, "success")
			}
			return res, nil
		}

		// Drain so the connection can be reused, then record the failure and
		// retry if we have budget left.
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		lastErr = fmt.Errorf("registry returned %s", res.Status)
		if operation != "" {
			metrics.RegisterRegistryRequest(host, operation, "retried")
		}
	}
	return nil, lastErr
}

func isTransientStatus(code int) bool {
	switch {
	case code >= http.StatusInternalServerError:
		return true
	case code == http.StatusTooManyRequests:
		return true
	case code == http.StatusUnauthorized, code == http.StatusForbidden, code == http.StatusNotFound:
		// Observed on registry oauth endpoints under load — retrying usually
		// clears the flake within a second. Bounded attempts + backoff keep
		// this from hiding genuinely-missing manifests for long.
		return true
	}
	return false
}

func backoffFor(attempt int) time.Duration {
	delay := backoffBase * (1 << (attempt - 1))
	if delay > backoffMax {
		delay = backoffMax
	}
	jitter := time.Duration(float64(delay) * jitterFactor * (rand.Float64()*2 - 1))
	return delay + jitter
}
