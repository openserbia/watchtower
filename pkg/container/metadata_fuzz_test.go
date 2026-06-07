package container

import (
	"strings"
	"testing"
)

// FuzzParseComposeDependsOn exercises the compose depends_on label parser with
// arbitrary input. The label value is read verbatim off a container's metadata
// (com.docker.compose.depends_on), so it is attacker-influenceable — a crafted
// or corrupt label must never panic the update scan. parseComposeDependsOn is
// total by design (unknown shapes are dropped silently), so the invariants we
// assert are structural rather than semantic:
//
//   - it never panics on any input;
//   - it never returns more entries than there are comma-separated fields;
//   - every returned dep has a non-empty, fully trimmed Service; and
//   - any Condition it sets is likewise trimmed.
func FuzzParseComposeDependsOn(f *testing.F) {
	seeds := []string{
		"",
		"web",
		"web,db",
		"web:service_healthy",
		"db:service_completed_successfully:required",
		"  web  ,  db  ",
		",,,",
		"::::",
		"web:service_started:false:extra",
		"a:b:c:d:e",
		strings.Repeat("a,", 1000),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		deps := parseComposeDependsOn(raw) // must not panic

		if maxEntries := strings.Count(raw, ",") + 1; len(deps) > maxEntries {
			t.Fatalf("parseComposeDependsOn(%q) returned %d deps, exceeds %d comma-separated entries",
				raw, len(deps), maxEntries)
		}

		for _, d := range deps {
			if d.Service == "" {
				t.Fatalf("parseComposeDependsOn(%q) produced a dep with an empty Service", raw)
			}
			if d.Service != strings.TrimSpace(d.Service) {
				t.Fatalf("parseComposeDependsOn(%q) produced an untrimmed Service %q", raw, d.Service)
			}
			if d.Condition != strings.TrimSpace(d.Condition) {
				t.Fatalf("parseComposeDependsOn(%q) produced an untrimmed Condition %q", raw, d.Condition)
			}
		}
	})
}
