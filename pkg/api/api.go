// Package api hosts the HTTP control-plane for watchtower (token auth,
// /v1/update, /v1/metrics).
package api

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strconv"

	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/pkg/metrics"
)

const (
	tokenMissingMsg = "api token is empty or has not been set. exiting"
	// DefaultListenAddr is used when --http-api-host isn't set. Binds to
	// every interface on port 8080, matching the pre-v1.12 behavior.
	DefaultListenAddr = ":8080"
)

// API is the http server responsible for serving the HTTP API endpoints
type API struct {
	Token             string
	ListenAddr        string
	hasHandlers       bool
	hasAuthedHandlers bool
}

// New is a factory function creating a new API instance
func New(token string) *API {
	return &API{
		Token:      token,
		ListenAddr: DefaultListenAddr,
	}
}

// RequireToken is wrapper around http.HandleFunc that checks token validity
// using a constant-time comparison so watchtower isn't a timing oracle for
// operators who expose :8080 to an untrusted network.
func (api *API) RequireToken(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		want := fmt.Sprintf("Bearer %s", api.Token)
		if subtle.ConstantTimeCompare([]byte(auth), []byte(want)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		log.Debug("Valid token found.")
		fn(w, r)
	}
}

// RegisterFunc is a wrapper around http.HandleFunc that also sets the flag used to determine whether to launch the API
func (api *API) RegisterFunc(path string, fn http.HandlerFunc) {
	api.hasHandlers = true
	api.hasAuthedHandlers = true
	http.HandleFunc(path, instrument(path, api.RequireToken(fn)))
}

// RegisterHandler is a wrapper around http.Handler that also sets the flag used to determine whether to launch the API
func (api *API) RegisterHandler(path string, handler http.Handler) {
	api.hasHandlers = true
	api.hasAuthedHandlers = true
	http.Handle(path, instrument(path, api.RequireToken(handler.ServeHTTP)))
}

// RegisterPublicHandler registers a handler without token auth. Used for the
// Prometheus /v1/metrics endpoint when --http-api-metrics-no-auth is set —
// Prometheus scraping is conventionally unauthenticated on trusted networks
// and bearer-token plumbing for every scraper is friction-for-no-gain in
// that setup. Network-level controls (localhost bind, reverse proxy,
// firewall) are expected to provide the real access boundary.
func (api *API) RegisterPublicHandler(path string, handler http.Handler) {
	api.hasHandlers = true
	http.Handle(path, instrument(path, handler.ServeHTTP))
}

// instrument wraps a handler so every request increments
// watchtower_api_requests_total with the response status. Uses a thin
// response-writer shim to capture the status after the handler has finished.
func instrument(path string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next(sw, r)
		metrics.RegisterAPIRequest(path, strconv.Itoa(sw.status))
	}
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write implicitly commits 200 if WriteHeader wasn't called — mirror that
// into the captured status so /v1/metrics-style handlers (which rely on the
// implicit 200) are counted correctly.
func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// Start the API and serve over HTTP. Requires an API Token to be set.
func (api *API) Start(block bool) error {
	if !api.hasHandlers {
		log.Debug("Watchtower HTTP API skipped.")
		return nil
	}

	if api.hasAuthedHandlers && api.Token == "" {
		log.Fatal(tokenMissingMsg)
	}

	addr := api.ListenAddr
	if addr == "" {
		addr = DefaultListenAddr
	}
	log.Debugf("Watchtower HTTP API listening on %s", addr)

	if block {
		runHTTPServer(addr)
	} else {
		go func() {
			runHTTPServer(addr)
		}()
	}
	return nil
}

func runHTTPServer(addr string) {
	log.Fatal(http.ListenAndServe(addr, nil))
}
