package mocks

// FilterableContainer is a hand-written stub implementing the
// filters.FilterableContainer interface for tests. Each field supplies the
// value(s) returned by the matching method.
type FilterableContainer struct {
	NameVal         string
	ImageNameVal    string
	IsWatchtowerVal bool
	EnabledVal      bool
	EnabledSet      bool
	ScopeVal        string
	ScopeSet        bool
}

func (m *FilterableContainer) Name() string { return m.NameVal }

func (m *FilterableContainer) ImageName() string { return m.ImageNameVal }

func (m *FilterableContainer) IsWatchtower() bool { return m.IsWatchtowerVal }

func (m *FilterableContainer) Enabled() (bool, bool) { return m.EnabledVal, m.EnabledSet }

func (m *FilterableContainer) Scope() (string, bool) { return m.ScopeVal, m.ScopeSet }
