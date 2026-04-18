// Package api hosts the HTTP control-plane for watchtower (token auth,
// /v1/update, /v1/metrics).
package api

import (
	"crypto/subtle"
	"fmt"
	"net/http"

	log "github.com/sirupsen/logrus"
)

const tokenMissingMsg = "api token is empty or has not been set. exiting"

// API is the http server responsible for serving the HTTP API endpoints
type API struct {
	Token             string
	hasHandlers       bool
	hasAuthedHandlers bool
}

// New is a factory function creating a new API instance
func New(token string) *API {
	return &API{
		Token: token,
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
	http.HandleFunc(path, api.RequireToken(fn))
}

// RegisterHandler is a wrapper around http.Handler that also sets the flag used to determine whether to launch the API
func (api *API) RegisterHandler(path string, handler http.Handler) {
	api.hasHandlers = true
	api.hasAuthedHandlers = true
	http.Handle(path, api.RequireToken(handler.ServeHTTP))
}

// RegisterPublicHandler registers a handler without token auth. Used for the
// Prometheus /v1/metrics endpoint when --http-api-metrics-no-auth is set —
// Prometheus scraping is conventionally unauthenticated on trusted networks
// and bearer-token plumbing for every scraper is friction-for-no-gain in
// that setup. Network-level controls (localhost bind, reverse proxy,
// firewall) are expected to provide the real access boundary.
func (api *API) RegisterPublicHandler(path string, handler http.Handler) {
	api.hasHandlers = true
	http.Handle(path, handler)
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

	if block {
		runHTTPServer()
	} else {
		go func() {
			runHTTPServer()
		}()
	}
	return nil
}

func runHTTPServer() {
	log.Fatal(http.ListenAndServe(":8080", nil))
}
