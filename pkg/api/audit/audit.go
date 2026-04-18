// Package audit exposes the HTTP handler for the /v1/audit endpoint.
//
// Unlike the log-based `--audit-unmanaged` warning (which tracks silent
// exclusions over time), this endpoint returns the full watch status of
// every container the Docker daemon currently reports: managed (label set
// to true), excluded (label set to false), and unmanaged (label absent).
// Intended as a pull-model alternative for operators who want to script
// post-deploy verification or dashboards without parsing logs.
package audit

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/pkg/container"
	"github.com/openserbia/watchtower/pkg/filters"
	"github.com/openserbia/watchtower/pkg/types"
)

// Path is the HTTP path the audit endpoint is served at.
const Path = "/v1/audit"

// Status captures how --label-enable treats a single container.
type Status string

const (
	// StatusManaged — operator set the enable label to true.
	StatusManaged Status = "managed"
	// StatusExcluded — operator set the enable label to false (intentional opt-out).
	StatusExcluded Status = "excluded"
	// StatusUnmanaged — no enable label at all. With --label-enable active,
	// these are silently skipped unless the operator notices them via
	// --audit-unmanaged or this endpoint.
	StatusUnmanaged Status = "unmanaged"
)

// Entry is a single container's audit line.
type Entry struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Status Status `json:"status"`
}

// Summary is a count-by-status digest returned alongside the full listing.
type Summary struct {
	Managed   int `json:"managed"`
	Excluded  int `json:"excluded"`
	Unmanaged int `json:"unmanaged"`
	Total     int `json:"total"`
}

// Report is the /v1/audit response envelope.
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	Scope       string    `json:"scope,omitempty"`
	Summary     Summary   `json:"summary"`
	Containers  []Entry   `json:"containers"`
}

// Handler serves the /v1/audit endpoint.
type Handler struct {
	Path   string
	client container.Client
	scope  string
}

// New returns a handler wired to the given Docker client and scope. Scope is
// propagated to the list filter so multi-scope operators only see their own
// containers when hitting the endpoint.
func New(client container.Client, scope string) *Handler {
	return &Handler{
		Path:   Path,
		client: client,
		scope:  scope,
	}
}

// Handle responds with the audit report as JSON. Errors during container
// enumeration return 500 with a short plain-text message so operators can
// distinguish "Docker socket unreachable" from "actually no containers".
func (h *Handler) Handle(w http.ResponseWriter, _ *http.Request) {
	filter := filters.NoFilter
	if h.scope != "" {
		filter = filters.FilterByScope(h.scope, filter)
	}
	containers, err := h.client.ListContainers(filter)
	if err != nil {
		log.WithError(err).Warn("audit: failed to list containers")
		http.Error(w, "failed to list containers: "+err.Error(), http.StatusInternalServerError)
		return
	}

	report := buildReport(containers, h.scope)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(report); err != nil {
		log.WithError(err).Warn("audit: failed to encode report")
	}
}

func buildReport(containers []types.Container, scope string) Report {
	report := Report{
		GeneratedAt: time.Now().UTC(),
		Scope:       scope,
		Containers:  make([]Entry, 0, len(containers)),
	}
	for _, c := range containers {
		if c.IsWatchtower() {
			continue
		}
		entry := Entry{
			Name:  c.Name(),
			Image: c.ImageName(),
		}
		enabled, labeled := c.Enabled()
		switch {
		case !labeled:
			entry.Status = StatusUnmanaged
			report.Summary.Unmanaged++
		case enabled:
			entry.Status = StatusManaged
			report.Summary.Managed++
		default:
			entry.Status = StatusExcluded
			report.Summary.Excluded++
		}
		report.Containers = append(report.Containers, entry)
	}
	report.Summary.Total = len(report.Containers)

	sort.Slice(report.Containers, func(i, j int) bool {
		return report.Containers[i].Name < report.Containers[j].Name
	})
	return report
}
