package flags

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

// These tests lock in the environment-parsing semantics that previously came
// from viper/cast, so the stdlib replacement in env.go stays behavior-compatible.

func TestEnvString(t *testing.T) {
	g := NewWithT(t)
	SetDefaults()

	g.Expect(envString("WATCHTOWER_LOG_LEVEL")).To(Equal("info"), "falls back to the registered default")

	t.Setenv("WATCHTOWER_LOG_LEVEL", "debug")
	g.Expect(envString("WATCHTOWER_LOG_LEVEL")).To(Equal("debug"), "process env overrides the default")

	g.Expect(envString("WT_UNSET_NO_DEFAULT")).To(Equal(""), "unset with no default is empty")
}

func TestEnvBool(t *testing.T) {
	g := NewWithT(t)
	SetDefaults()

	g.Expect(envBool("WT_UNSET_BOOL")).To(BeFalse(), "unset is false")

	for value, want := range map[string]bool{
		"true": true, "TRUE": true, "1": true, "t": true,
		"false": false, "0": false, "maybe": false, "": false,
	} {
		t.Setenv("WT_TEST_BOOL", value)
		g.Expect(envBool("WT_TEST_BOOL")).To(Equal(want), "ParseBool(%q)", value)
	}
}

func TestEnvInt(t *testing.T) {
	g := NewWithT(t)
	SetDefaults()

	g.Expect(envInt("WATCHTOWER_POLL_INTERVAL")).To(Equal(defaultInterval), "uses the registered default")

	t.Setenv("WATCHTOWER_POLL_INTERVAL", "300")
	g.Expect(envInt("WATCHTOWER_POLL_INTERVAL")).To(Equal(300))

	t.Setenv("WT_TEST_INT", "not-a-number")
	g.Expect(envInt("WT_TEST_INT")).To(Equal(0), "unparseable yields zero")
}

func TestEnvDuration(t *testing.T) {
	g := NewWithT(t)
	SetDefaults()

	g.Expect(envDuration("WATCHTOWER_TIMEOUT")).
		To(Equal(time.Second*defaultStopTimeoutSeconds), "uses the registered default")

	t.Setenv("WATCHTOWER_TIMEOUT", "30s")
	g.Expect(envDuration("WATCHTOWER_TIMEOUT")).To(Equal(30 * time.Second))

	// A bare number is interpreted as nanoseconds, matching the prior cast behavior.
	t.Setenv("WT_TEST_DUR", "100")
	g.Expect(envDuration("WT_TEST_DUR")).To(Equal(100 * time.Nanosecond))

	// Falls back when unset and a fallback is supplied.
	g.Expect(envDuration("WT_UNSET_DUR", 5*time.Minute)).To(Equal(5 * time.Minute))
}

func TestEnvStringSlice(t *testing.T) {
	g := NewWithT(t)
	SetDefaults()

	g.Expect(envStringSlice("WATCHTOWER_NOTIFICATIONS")).To(BeEmpty(), "unset is an empty slice")

	t.Setenv("WATCHTOWER_NOTIFICATIONS", "slack email")
	g.Expect(envStringSlice("WATCHTOWER_NOTIFICATIONS")).
		To(Equal([]string{"slack", "email"}), "splits on whitespace")

	// Commas are NOT split (preserves the viper#380 behavior the disable-containers
	// flag works around explicitly).
	t.Setenv("WATCHTOWER_NOTIFICATIONS", "slack,email")
	g.Expect(envStringSlice("WATCHTOWER_NOTIFICATIONS")).To(Equal([]string{"slack,email"}))
}

func TestEnvIsSetAndAllEnvKeys(t *testing.T) {
	g := NewWithT(t)
	SetDefaults()

	g.Expect(envIsSet("NO_COLOR")).To(BeFalse(), "unset")
	t.Setenv("NO_COLOR", "")
	g.Expect(envIsSet("NO_COLOR")).To(BeTrue(), "present even when empty (no-color.org)")

	keys := AllEnvKeys()
	g.Expect(keys).To(ContainElement("WATCHTOWER_NOTIFICATIONS"), "empty-slice default is still a known key")
	g.Expect(keys).To(ContainElement("WATCHTOWER_LOG_FORMAT"))
	g.Expect(keys).NotTo(ContainElement("NO_COLOR"), "envIsSet must not register the key (matches viper.AllKeys)")
}
