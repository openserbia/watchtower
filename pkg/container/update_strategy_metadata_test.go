package container

import (
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = ginkgo.Describe("update-strategy metadata", func() {
	ginkgo.Describe("UpdateStrategyLabel", func() {
		ginkgo.DescribeTable(
			"resolves the label value",
			func(value, expected string, expectedOK bool) {
				c := MockContainer(WithLabels(map[string]string{
					"com.centurylinklabs.watchtower.update-strategy": value,
				}))
				got, ok := c.UpdateStrategyLabel()
				Expect(ok).To(Equal(expectedOK))
				Expect(got).To(Equal(expected))
			},
			ginkgo.Entry("recreate", "recreate", "recreate", true),
			ginkgo.Entry("rolling-restart", "rolling-restart", "rolling-restart", true),
			ginkgo.Entry("blue-green", "blue-green", "blue-green", true),
			ginkgo.Entry("uppercase is normalized", "Blue-Green", "blue-green", true),
			ginkgo.Entry("surrounding whitespace is trimmed", "  recreate  ", "recreate", true),
			ginkgo.Entry("unknown value", "rolling", "", false),
			ginkgo.Entry("empty value", "", "", false),
		)

		ginkgo.It("returns (\"\", false) when the label is absent", func() {
			c := MockContainer(WithLabels(map[string]string{}))
			got, ok := c.UpdateStrategyLabel()
			Expect(ok).To(BeFalse())
			Expect(got).To(BeEmpty())
		})
	})

	ginkgo.Describe("BlueGreenDrain", func() {
		ginkgo.DescribeTable(
			"resolves the label value",
			func(value string, expected time.Duration, expectedOK bool) {
				c := MockContainer(WithLabels(map[string]string{
					"com.centurylinklabs.watchtower.blue-green.drain": value,
				}))
				got, ok := c.BlueGreenDrain()
				Expect(ok).To(Equal(expectedOK))
				Expect(got).To(Equal(expected))
			},
			ginkgo.Entry("valid duration", "30s", 30*time.Second, true),
			ginkgo.Entry("zero is allowed and means no drain", "0s", time.Duration(0), true),
			ginkgo.Entry("negative is rejected", "-5s", time.Duration(0), false),
			ginkgo.Entry("unparseable is rejected", "soon", time.Duration(0), false),
			ginkgo.Entry("empty value is rejected", "", time.Duration(0), false),
		)

		ginkgo.It("returns (0, false) when the label is absent", func() {
			c := MockContainer(WithLabels(map[string]string{}))
			got, ok := c.BlueGreenDrain()
			Expect(ok).To(BeFalse())
			Expect(got).To(Equal(time.Duration(0)))
		})
	})
})
