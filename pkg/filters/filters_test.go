package filters

import (
	"testing"

	. "github.com/onsi/gomega"

	"github.com/openserbia/watchtower/pkg/container/mocks"
)

func TestWatchtowerContainersFilter(t *testing.T) {
	g := NewWithT(t)
	container := &mocks.FilterableContainer{IsWatchtowerVal: true}

	g.Expect(WatchtowerContainersFilter(container)).To(BeTrue())
}

func TestNoFilter(t *testing.T) {
	g := NewWithT(t)
	container := &mocks.FilterableContainer{}

	g.Expect(NoFilter(container)).To(BeTrue())
}

func TestFilterByNames(t *testing.T) {
	g := NewWithT(t)

	var names []string

	filter := FilterByNames(names, nil)
	g.Expect(filter).To(BeNil())

	names = append(names, "test")

	filter = FilterByNames(names, NoFilter)
	g.Expect(filter).ToNot(BeNil())

	g.Expect(filter(&mocks.FilterableContainer{NameVal: "test"})).To(BeTrue())
	g.Expect(filter(&mocks.FilterableContainer{NameVal: "NoTest"})).To(BeFalse())
}

func TestFilterByNamesRegex(t *testing.T) {
	g := NewWithT(t)
	names := []string{`ba(b|ll)oon`}

	filter := FilterByNames(names, NoFilter)
	g.Expect(filter).ToNot(BeNil())

	g.Expect(filter(&mocks.FilterableContainer{NameVal: "balloon"})).To(BeTrue())
	g.Expect(filter(&mocks.FilterableContainer{NameVal: "spoon"})).To(BeFalse())
	g.Expect(filter(&mocks.FilterableContainer{NameVal: "baboonious"})).To(BeFalse())
}

func TestFilterByEnableLabel(t *testing.T) {
	g := NewWithT(t)
	filter := FilterByEnableLabel(NoFilter)
	g.Expect(filter).ToNot(BeNil())

	g.Expect(filter(&mocks.FilterableContainer{EnabledVal: true, EnabledSet: true})).To(BeTrue())
	g.Expect(filter(&mocks.FilterableContainer{EnabledVal: false, EnabledSet: true})).To(BeTrue())
	g.Expect(filter(&mocks.FilterableContainer{EnabledVal: false, EnabledSet: false})).To(BeFalse())
}

func TestFilterByScope(t *testing.T) {
	g := NewWithT(t)
	scope := "testscope"

	filter := FilterByScope(scope, NoFilter)
	g.Expect(filter).ToNot(BeNil())

	g.Expect(filter(&mocks.FilterableContainer{ScopeVal: "testscope", ScopeSet: true})).To(BeTrue())
	g.Expect(filter(&mocks.FilterableContainer{ScopeVal: "nottestscope", ScopeSet: true})).To(BeFalse())
	g.Expect(filter(&mocks.FilterableContainer{ScopeVal: "", ScopeSet: false})).To(BeFalse())
}

func TestFilterByNoneScope(t *testing.T) {
	g := NewWithT(t)
	scope := "none"

	filter := FilterByScope(scope, NoFilter)
	g.Expect(filter).ToNot(BeNil())

	g.Expect(filter(&mocks.FilterableContainer{ScopeVal: "anyscope", ScopeSet: true})).To(BeFalse())
	g.Expect(filter(&mocks.FilterableContainer{ScopeVal: "", ScopeSet: false})).To(BeTrue())
	g.Expect(filter(&mocks.FilterableContainer{ScopeVal: "", ScopeSet: true})).To(BeTrue())
	g.Expect(filter(&mocks.FilterableContainer{ScopeVal: "none", ScopeSet: true})).To(BeTrue())
}

func TestBuildFilterNoneScope(t *testing.T) {
	g := NewWithT(t)
	filter, desc := BuildFilter(nil, nil, false, "none")

	g.Expect(desc).To(ContainSubstring("without a scope"))

	scoped := &mocks.FilterableContainer{ScopeVal: "anyscope", ScopeSet: true}
	unscoped := &mocks.FilterableContainer{ScopeVal: "", ScopeSet: false}

	g.Expect(filter(scoped)).To(BeFalse())
	g.Expect(filter(unscoped)).To(BeTrue())
}

func TestFilterByDisabledLabel(t *testing.T) {
	g := NewWithT(t)
	filter := FilterByDisabledLabel(NoFilter)
	g.Expect(filter).ToNot(BeNil())

	g.Expect(filter(&mocks.FilterableContainer{EnabledVal: true, EnabledSet: true})).To(BeTrue())
	g.Expect(filter(&mocks.FilterableContainer{EnabledVal: false, EnabledSet: true})).To(BeFalse())
	g.Expect(filter(&mocks.FilterableContainer{EnabledVal: false, EnabledSet: false})).To(BeTrue())
}

func TestFilterByImage(t *testing.T) {
	g := NewWithT(t)
	filterEmpty := FilterByImage(nil, NoFilter)
	filterSingle := FilterByImage([]string{"registry"}, NoFilter)
	filterMultiple := FilterByImage([]string{"registry", "bla"}, NoFilter)
	g.Expect(filterSingle).ToNot(BeNil())
	g.Expect(filterMultiple).ToNot(BeNil())

	container := &mocks.FilterableContainer{ImageNameVal: "registry:2"}
	g.Expect(filterEmpty(container)).To(BeTrue())
	g.Expect(filterSingle(container)).To(BeTrue())
	g.Expect(filterMultiple(container)).To(BeTrue())

	container = &mocks.FilterableContainer{ImageNameVal: "registry:latest"}
	g.Expect(filterEmpty(container)).To(BeTrue())
	g.Expect(filterSingle(container)).To(BeTrue())
	g.Expect(filterMultiple(container)).To(BeTrue())

	container = &mocks.FilterableContainer{ImageNameVal: "abcdef1234"}
	g.Expect(filterEmpty(container)).To(BeTrue())
	g.Expect(filterSingle(container)).To(BeFalse())
	g.Expect(filterMultiple(container)).To(BeFalse())

	container = &mocks.FilterableContainer{ImageNameVal: "bla:latest"}
	g.Expect(filterEmpty(container)).To(BeTrue())
	g.Expect(filterSingle(container)).To(BeFalse())
	g.Expect(filterMultiple(container)).To(BeTrue())
}

func TestBuildFilter(t *testing.T) {
	g := NewWithT(t)
	names := []string{"test", "valid"}

	filter, desc := BuildFilter(names, []string{}, false, "")
	g.Expect(desc).To(ContainSubstring("test"))
	g.Expect(desc).To(ContainSubstring("or"))
	g.Expect(desc).To(ContainSubstring("valid"))

	g.Expect(filter(&mocks.FilterableContainer{NameVal: "Invalid"})).To(BeFalse())
	g.Expect(filter(&mocks.FilterableContainer{NameVal: "test"})).To(BeTrue())
	g.Expect(filter(&mocks.FilterableContainer{NameVal: "Invalid", EnabledVal: true, EnabledSet: true})).To(BeFalse())
	g.Expect(filter(&mocks.FilterableContainer{NameVal: "test", EnabledVal: true, EnabledSet: true})).To(BeTrue())
	g.Expect(filter(&mocks.FilterableContainer{EnabledVal: false, EnabledSet: true})).To(BeFalse())
}

func TestBuildFilterEnableLabel(t *testing.T) {
	g := NewWithT(t)

	var names []string
	names = append(names, "test")

	filter, desc := BuildFilter(names, []string{}, true, "")
	g.Expect(desc).To(ContainSubstring("using enable label"))

	g.Expect(filter(&mocks.FilterableContainer{EnabledVal: false, EnabledSet: false})).To(BeFalse())
	g.Expect(filter(&mocks.FilterableContainer{NameVal: "Invalid", EnabledVal: true, EnabledSet: true})).To(BeFalse())
	g.Expect(filter(&mocks.FilterableContainer{NameVal: "test", EnabledVal: true, EnabledSet: true})).To(BeTrue())
	g.Expect(filter(&mocks.FilterableContainer{EnabledVal: false, EnabledSet: true})).To(BeFalse())
}

func TestBuildFilterDisableContainer(t *testing.T) {
	g := NewWithT(t)
	filter, desc := BuildFilter([]string{}, []string{"excluded", "notfound"}, false, "")
	g.Expect(desc).To(ContainSubstring("not named"))
	g.Expect(desc).To(ContainSubstring("excluded"))
	g.Expect(desc).To(ContainSubstring("or"))
	g.Expect(desc).To(ContainSubstring("notfound"))

	g.Expect(filter(&mocks.FilterableContainer{NameVal: "Another"})).To(BeTrue())
	g.Expect(filter(&mocks.FilterableContainer{NameVal: "AnotherOne", EnabledVal: true, EnabledSet: true})).To(BeTrue())
	g.Expect(filter(&mocks.FilterableContainer{NameVal: "test"})).To(BeTrue())
	g.Expect(filter(&mocks.FilterableContainer{NameVal: "excluded", EnabledVal: true, EnabledSet: true})).To(BeFalse())
	g.Expect(filter(&mocks.FilterableContainer{NameVal: "excludedAsSubstring", EnabledVal: true, EnabledSet: true})).To(BeTrue())
	g.Expect(filter(&mocks.FilterableContainer{NameVal: "notfound", EnabledVal: true, EnabledSet: true})).To(BeFalse())
	g.Expect(filter(&mocks.FilterableContainer{EnabledVal: false, EnabledSet: true})).To(BeFalse())
}
