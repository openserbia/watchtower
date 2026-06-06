package metrics_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/openserbia/watchtower/pkg/metrics"
	"github.com/openserbia/watchtower/pkg/session"
)

var _ = Describe("NewMetric", func() {
	// Regression: actions.Update returns a nil report on its error paths (e.g.
	// the Docker daemon went unreachable mid-scan and ListContainers failed).
	// The scheduled/HTTP/event callers feed that result straight into NewMetric,
	// so a nil report used to dereference inside the cron goroutine and crash
	// the whole daemon with a SIGSEGV.
	It("returns an empty metric for a nil report instead of panicking", func() {
		var metric *metrics.Metric
		Expect(func() { metric = metrics.NewMetric(nil) }).NotTo(Panic())
		Expect(metric).NotTo(BeNil())
		Expect(metric.Scanned).To(Equal(0))
		Expect(metric.Updated).To(Equal(0))
		Expect(metric.Failed).To(Equal(0))
	})

	It("returns zero counts for an empty report", func() {
		metric := metrics.NewMetric(session.NewReport(session.Progress{}))
		Expect(metric.Scanned).To(Equal(0))
		Expect(metric.Updated).To(Equal(0))
		Expect(metric.Failed).To(Equal(0))
	})
})
