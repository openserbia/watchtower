package container

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ComposeDependencies / ComposeInitDependencies", func() {
	When("the container has no compose depends_on label", func() {
		It("returns nil from both methods", func() {
			c := MockContainer()
			Expect(c.ComposeDependencies()).To(BeNil())
			Expect(c.ComposeInitDependencies()).To(BeNil())
		})
	})

	When("depends_on lists service names only (no conditions)", func() {
		It("returns all services from ComposeDependencies and none from init", func() {
			c := MockContainer(WithLabels(map[string]string{
				"com.docker.compose.depends_on": "pg-ready,migrate",
			}))
			Expect(c.ComposeDependencies()).To(Equal([]string{"pg-ready", "migrate"}))
			Expect(c.ComposeInitDependencies()).To(BeEmpty())
		})
	})

	When("depends_on includes service_completed_successfully entries", func() {
		It("ComposeInitDependencies returns only those entries", func() {
			c := MockContainer(WithLabels(map[string]string{
				"com.docker.compose.depends_on": "pg-ready:service_completed_successfully:true,migrate:service_completed_successfully:true",
			}))
			Expect(c.ComposeDependencies()).To(Equal([]string{"pg-ready", "migrate"}))
			Expect(c.ComposeInitDependencies()).To(Equal([]string{"pg-ready", "migrate"}))
		})
	})

	When("depends_on mixes init containers and long-running peers", func() {
		It("excludes service_started and service_healthy from init deps", func() {
			c := MockContainer(WithLabels(map[string]string{
				"com.docker.compose.depends_on": "pg:service_started:true,migrate:service_completed_successfully:true,cache:service_healthy:true",
			}))
			Expect(c.ComposeDependencies()).To(Equal([]string{"pg", "migrate", "cache"}))
			Expect(c.ComposeInitDependencies()).To(Equal([]string{"migrate"}))
		})
	})

	When("depends_on has leading/trailing whitespace and empty entries", func() {
		It("trims and drops empties", func() {
			c := MockContainer(WithLabels(map[string]string{
				"com.docker.compose.depends_on": " migrate : service_completed_successfully : true , ,  pg : service_started : true ",
			}))
			Expect(c.ComposeInitDependencies()).To(Equal([]string{"migrate"}))
			Expect(c.ComposeDependencies()).To(Equal([]string{"migrate", "pg"}))
		})
	})

	When("depends_on entry is missing the condition modifier", func() {
		It("falls back to graph membership only; init-deps stays empty", func() {
			c := MockContainer(WithLabels(map[string]string{
				"com.docker.compose.depends_on": "bare-dep",
			}))
			Expect(c.ComposeDependencies()).To(Equal([]string{"bare-dep"}))
			Expect(c.ComposeInitDependencies()).To(BeEmpty())
		})
	})
})
