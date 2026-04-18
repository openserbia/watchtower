// Package update exposes the HTTP handler for the /v1/update API endpoint.
package update

import (
	"encoding/json"
	"net/http"
	"strings"

	log "github.com/sirupsen/logrus"
)

// Response is the JSON body returned by the /v1/update handler. Status is
// always populated; Scanned/Updated/Failed are only meaningful when
// Status == "completed".
type Response struct {
	// Status is one of "completed" or "skipped".
	Status string `json:"status"`
	// Reason explains why the request was skipped. Only set when
	// Status == "skipped".
	Reason string `json:"reason,omitempty"`
	// Scanned is the number of containers inspected during the update.
	Scanned int `json:"scanned,omitempty"`
	// Updated is the number of containers recreated with a newer image.
	Updated int `json:"updated,omitempty"`
	// Failed is the number of containers whose update failed.
	Failed int `json:"failed,omitempty"`
}

var lock chan bool

// New is a factory function creating a new Handler instance. The updateFn
// callback returns a Response that is serialised as the HTTP response body,
// so scripts driving the endpoint can tell how the scan went without
// scraping logs.
func New(updateFn func(images []string) Response, updateLock chan bool) *Handler {
	if updateLock != nil {
		lock = updateLock
	} else {
		lock = make(chan bool, 1)
		lock <- true
	}

	return &Handler{
		fn:   updateFn,
		Path: "/v1/update",
	}
}

// Handler is an API handler used for triggering container update scans
type Handler struct {
	fn   func(images []string) Response
	Path string
}

// Handle is the actual http.Handle function doing all the heavy lifting.
// Returns 200 with a JSON Response body on success; 429 with a "skipped"
// body when another update is already running. Targeted updates (an
// `image=` query parameter) block until the in-flight update completes
// rather than 429-ing, because the caller almost always wants the
// specified images updated eventually.
func (handle *Handler) Handle(w http.ResponseWriter, r *http.Request) {
	log.Info("Updates triggered by HTTP API request.")

	var images []string
	imageQueries, found := r.URL.Query()["image"]
	if found {
		for _, image := range imageQueries {
			images = append(images, strings.Split(image, ",")...)
		}
	}

	if len(images) > 0 {
		chanValue := <-lock
		defer func() { lock <- chanValue }()
		writeJSON(w, http.StatusOK, handle.fn(images))
		return
	}

	select {
	case chanValue := <-lock:
		defer func() { lock <- chanValue }()
		writeJSON(w, http.StatusOK, handle.fn(images))
	default:
		log.Debug("Skipped. Another update already running.")
		writeJSON(w, http.StatusTooManyRequests, Response{
			Status: "skipped",
			Reason: "another update is already running",
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, body Response) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.WithError(err).Warn("failed to encode /v1/update response")
	}
}
