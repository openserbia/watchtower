package flags

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"
)

// newStrategyFlagSet builds a freshly-registered command, mirroring the wiring
// cmd/root.go performs before ProcessFlagAliases runs.
func newStrategyFlagSet() *cobra.Command {
	cmd := new(cobra.Command)
	SetDefaults()
	RegisterDockerFlags(cmd)
	RegisterSystemFlags(cmd)
	RegisterNotificationFlags(cmd)

	return cmd
}

func TestUpdateStrategyDefaultsToRecreate(t *testing.T) {
	g := NewWithT(t)
	cmd := newStrategyFlagSet()
	g.Expect(cmd.ParseFlags([]string{})).To(Succeed())
	flags := cmd.Flags()
	ProcessFlagAliases(flags)

	strategy, _ := flags.GetString("update-strategy")
	g.Expect(strategy).To(Equal("recreate"))
}

func TestRollingRestartAliasResolvesToRollingRestartStrategy(t *testing.T) {
	g := NewWithT(t)
	cmd := newStrategyFlagSet()
	g.Expect(cmd.ParseFlags([]string{"--rolling-restart"})).To(Succeed())
	flags := cmd.Flags()
	ProcessFlagAliases(flags)

	strategy, _ := flags.GetString("update-strategy")
	g.Expect(strategy).To(Equal("rolling-restart"))
}

func TestRollingRestartEnvResolvesToRollingRestartStrategy(t *testing.T) {
	g := NewWithT(t)
	t.Setenv("WATCHTOWER_ROLLING_RESTART", "true")
	cmd := newStrategyFlagSet()
	g.Expect(cmd.ParseFlags([]string{})).To(Succeed())
	flags := cmd.Flags()
	ProcessFlagAliases(flags)

	strategy, _ := flags.GetString("update-strategy")
	g.Expect(strategy).To(Equal("rolling-restart"))
}

func TestUpdateStrategyBlueGreenIsPreserved(t *testing.T) {
	g := NewWithT(t)
	cmd := newStrategyFlagSet()
	g.Expect(cmd.ParseFlags([]string{"--update-strategy", "blue-green"})).To(Succeed())
	flags := cmd.Flags()
	ProcessFlagAliases(flags)

	strategy, _ := flags.GetString("update-strategy")
	g.Expect(strategy).To(Equal("blue-green"))
}

func TestUpdateStrategyFromEnv(t *testing.T) {
	g := NewWithT(t)
	t.Setenv("WATCHTOWER_UPDATE_STRATEGY", "blue-green")
	cmd := newStrategyFlagSet()
	g.Expect(cmd.ParseFlags([]string{})).To(Succeed())
	flags := cmd.Flags()
	ProcessFlagAliases(flags)

	strategy, _ := flags.GetString("update-strategy")
	g.Expect(strategy).To(Equal("blue-green"))
}

func TestBlueGreenDrainDefaultsToTenSeconds(t *testing.T) {
	g := NewWithT(t)
	cmd := newStrategyFlagSet()
	g.Expect(cmd.ParseFlags([]string{})).To(Succeed())
	flags := cmd.Flags()
	ProcessFlagAliases(flags)

	drain, _ := flags.GetDuration("blue-green-drain")
	g.Expect(drain).To(Equal(10 * time.Second))
}

func TestBlueGreenDrainParsesExplicitValue(t *testing.T) {
	g := NewWithT(t)
	cmd := newStrategyFlagSet()
	g.Expect(cmd.ParseFlags([]string{"--blue-green-drain", "45s"})).To(Succeed())
	flags := cmd.Flags()
	ProcessFlagAliases(flags)

	drain, _ := flags.GetDuration("blue-green-drain")
	g.Expect(drain).To(Equal(45 * time.Second))
}

func TestBlueGreenDrainParsesFromEnv(t *testing.T) {
	g := NewWithT(t)
	t.Setenv("WATCHTOWER_BLUE_GREEN_DRAIN", "1m")
	cmd := newStrategyFlagSet()
	g.Expect(cmd.ParseFlags([]string{})).To(Succeed())
	flags := cmd.Flags()
	ProcessFlagAliases(flags)

	drain, _ := flags.GetDuration("blue-green-drain")
	g.Expect(drain).To(Equal(time.Minute))
}

// NOTE: the conflict case (--rolling-restart together with an incompatible
// --update-strategy) and the unknown-enum case both call log.Fatalf, which
// terminates the process. The design explicitly keeps these as Fatalf rather
// than error returns; rather than refactor the production signature, those two
// branches are left unasserted here.
