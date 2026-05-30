package flags

import (
	"os"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestEnvConfig_Defaults(t *testing.T) {
	g := NewWithT(t)
	// Unset testing environments own variables, since those are not what is under test
	_ = os.Unsetenv("DOCKER_TLS_VERIFY")
	_ = os.Unsetenv("DOCKER_HOST")

	cmd := new(cobra.Command)
	SetDefaults()
	RegisterDockerFlags(cmd)

	g.Expect(EnvConfig(cmd)).To(Succeed())

	g.Expect(os.Getenv("DOCKER_HOST")).To(Equal("unix:///var/run/docker.sock"))
	g.Expect(os.Getenv("DOCKER_TLS_VERIFY")).To(Equal(""))
	// Re-enable this test when we've moved to github actions.
	// g.Expect(os.Getenv("DOCKER_API_VERSION")).To(Equal(DockerAPIMinVersion))
}

func TestEnvConfig_Custom(t *testing.T) {
	g := NewWithT(t)
	cmd := new(cobra.Command)
	SetDefaults()
	RegisterDockerFlags(cmd)

	g.Expect(cmd.ParseFlags([]string{"--host", "some-custom-docker-host", "--tlsverify", "--api-version", "1.99"})).
		To(Succeed())

	g.Expect(EnvConfig(cmd)).To(Succeed())

	g.Expect(os.Getenv("DOCKER_HOST")).To(Equal("some-custom-docker-host"))
	g.Expect(os.Getenv("DOCKER_TLS_VERIFY")).To(Equal("1"))
	// Re-enable this test when we've moved to github actions.
	// g.Expect(os.Getenv("DOCKER_API_VERSION")).To(Equal("1.99"))
}

func TestGetSecretsFromFilesWithString(t *testing.T) {
	value := "supersecretstring"
	t.Setenv("WATCHTOWER_NOTIFICATION_EMAIL_SERVER_PASSWORD", value)

	testGetSecretsFromFiles(t, "notification-email-server-password", value)
}

func TestGetSecretsFromFilesWithFile(t *testing.T) {
	g := NewWithT(t)
	value := "megasecretstring"

	// Create the temporary file which will contain a secret.
	file, err := os.CreateTemp(t.TempDir(), "watchtower-")
	g.Expect(err).NotTo(HaveOccurred())

	// Write the secret to the temporary file.
	_, err = file.WriteString(value)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(file.Close()).To(Succeed())

	t.Setenv("WATCHTOWER_NOTIFICATION_EMAIL_SERVER_PASSWORD", file.Name())

	testGetSecretsFromFiles(t, "notification-email-server-password", value)
}

func TestGetSliceSecretsFromFiles(t *testing.T) {
	g := NewWithT(t)
	values := []string{"entry2", "", "entry3"}

	// Create the temporary file which will contain a secret.
	file, err := os.CreateTemp(t.TempDir(), "watchtower-")
	g.Expect(err).NotTo(HaveOccurred())

	// Write the secret to the temporary file.
	for _, value := range values {
		_, err = file.WriteString("\n" + value)
		g.Expect(err).NotTo(HaveOccurred())
	}
	g.Expect(file.Close()).To(Succeed())

	testGetSecretsFromFiles(t, "notification-url", `[entry1,entry2,entry3]`,
		`--notification-url`, "entry1",
		`--notification-url`, file.Name())
}

func testGetSecretsFromFiles(t *testing.T, flagName, expected string, args ...string) {
	t.Helper()

	g := NewWithT(t)
	cmd := new(cobra.Command)
	SetDefaults()
	RegisterSystemFlags(cmd)
	RegisterNotificationFlags(cmd)
	g.Expect(cmd.ParseFlags(args)).To(Succeed())
	GetSecretsFromFiles(cmd)
	flag := cmd.PersistentFlags().Lookup(flagName)
	g.Expect(flag).NotTo(BeNil())
	value := flag.Value.String()

	g.Expect(value).To(Equal(expected))
}

func TestHTTPAPIPeriodicPollsFlag(t *testing.T) {
	g := NewWithT(t)
	cmd := new(cobra.Command)
	SetDefaults()
	RegisterDockerFlags(cmd)
	RegisterSystemFlags(cmd)

	g.Expect(cmd.ParseFlags([]string{"--http-api-periodic-polls"})).To(Succeed())

	periodicPolls, err := cmd.PersistentFlags().GetBool("http-api-periodic-polls")
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(periodicPolls).To(BeTrue())
}

func TestIsFile(t *testing.T) {
	g := NewWithT(t)
	g.Expect(isFile("https://google.com")).To(BeFalse(), "an URL should never be considered a file")
	g.Expect(isFile(os.Args[0])).To(BeTrue(), "the currently running binary path should always be considered a file")
}

func TestProcessFlagAliases(t *testing.T) {
	g := NewWithT(t)
	logrus.StandardLogger().ExitFunc = func(_ int) { t.FailNow() }
	cmd := new(cobra.Command)
	SetDefaults()
	RegisterDockerFlags(cmd)
	RegisterSystemFlags(cmd)
	RegisterNotificationFlags(cmd)

	g.Expect(cmd.ParseFlags([]string{
		`--porcelain`, `v1`,
		`--interval`, `10`,
		`--trace`,
	})).To(Succeed())
	flags := cmd.Flags()
	ProcessFlagAliases(flags)

	urls, _ := flags.GetStringArray(`notification-url`)
	g.Expect(urls).To(ContainElement(`logger://`))

	logStdout, _ := flags.GetBool(`notification-log-stdout`)
	g.Expect(logStdout).To(BeTrue())

	report, _ := flags.GetBool(`notification-report`)
	g.Expect(report).To(BeTrue())

	template, _ := flags.GetString(`notification-template`)
	g.Expect(template).To(Equal(`porcelain.v1.summary-no-log`))

	sched, _ := flags.GetString(`schedule`)
	g.Expect(sched).To(Equal(`@every 10s`))

	logLevel, _ := flags.GetString(`log-level`)
	g.Expect(logLevel).To(Equal(`trace`))
}

func TestProcessFlagAliasesLogLevelFromEnvironment(t *testing.T) {
	g := NewWithT(t)
	cmd := new(cobra.Command)
	t.Setenv("WATCHTOWER_DEBUG", `true`)

	SetDefaults()
	RegisterDockerFlags(cmd)
	RegisterSystemFlags(cmd)
	RegisterNotificationFlags(cmd)

	g.Expect(cmd.ParseFlags([]string{})).To(Succeed())
	flags := cmd.Flags()
	ProcessFlagAliases(flags)

	logLevel, _ := flags.GetString(`log-level`)
	g.Expect(logLevel).To(Equal(`debug`))
}

func TestLogFormatFlag(t *testing.T) {
	g := NewWithT(t)
	cmd := new(cobra.Command)

	SetDefaults()
	RegisterDockerFlags(cmd)
	RegisterSystemFlags(cmd)

	// Ensure the default value is Auto
	g.Expect(cmd.ParseFlags([]string{})).To(Succeed())
	g.Expect(SetupLogging(cmd.Flags())).To(Succeed())
	g.Expect(logrus.StandardLogger().Formatter).To(BeAssignableToTypeOf(&logrus.TextFormatter{}))

	// Test JSON format
	g.Expect(cmd.ParseFlags([]string{`--log-format`, `JSON`})).To(Succeed())
	g.Expect(SetupLogging(cmd.Flags())).To(Succeed())
	g.Expect(logrus.StandardLogger().Formatter).To(BeAssignableToTypeOf(&logrus.JSONFormatter{}))

	// Test Pretty format
	g.Expect(cmd.ParseFlags([]string{`--log-format`, `pretty`})).To(Succeed())
	g.Expect(SetupLogging(cmd.Flags())).To(Succeed())
	g.Expect(logrus.StandardLogger().Formatter).To(BeAssignableToTypeOf(&logrus.TextFormatter{}))
	textFormatter, ok := logrus.StandardLogger().Formatter.(*logrus.TextFormatter)
	g.Expect(ok).To(BeTrue())
	g.Expect(textFormatter.ForceColors).To(BeTrue())
	g.Expect(textFormatter.FullTimestamp).To(BeFalse())

	// Test LogFmt format
	g.Expect(cmd.ParseFlags([]string{`--log-format`, `logfmt`})).To(Succeed())
	g.Expect(SetupLogging(cmd.Flags())).To(Succeed())
	textFormatter, ok = logrus.StandardLogger().Formatter.(*logrus.TextFormatter)
	g.Expect(ok).To(BeTrue())
	g.Expect(textFormatter.DisableColors).To(BeTrue())
	g.Expect(textFormatter.FullTimestamp).To(BeTrue())

	// Test invalid format
	g.Expect(cmd.ParseFlags([]string{`--log-format`, `cowsay`})).To(Succeed())
	g.Expect(SetupLogging(cmd.Flags())).NotTo(Succeed())
}

func TestLogLevelFlag(t *testing.T) {
	g := NewWithT(t)
	cmd := new(cobra.Command)

	SetDefaults()
	RegisterDockerFlags(cmd)
	RegisterSystemFlags(cmd)

	// Test invalid format
	g.Expect(cmd.ParseFlags([]string{`--log-level`, `gossip`})).To(Succeed())
	g.Expect(SetupLogging(cmd.Flags())).NotTo(Succeed())
}

func TestProcessFlagAliasesSchedAndInterval(t *testing.T) {
	g := NewWithT(t)
	logrus.StandardLogger().ExitFunc = func(_ int) { panic(`FATAL`) }
	cmd := new(cobra.Command)
	SetDefaults()
	RegisterDockerFlags(cmd)
	RegisterSystemFlags(cmd)
	RegisterNotificationFlags(cmd)

	g.Expect(cmd.ParseFlags([]string{`--schedule`, `@hourly`, `--interval`, `10`})).To(Succeed())
	flags := cmd.Flags()

	g.Expect(func() {
		ProcessFlagAliases(flags)
	}).To(PanicWith(`FATAL`))
}

func TestProcessFlagAliasesScheduleFromEnvironment(t *testing.T) {
	g := NewWithT(t)
	cmd := new(cobra.Command)

	t.Setenv("WATCHTOWER_SCHEDULE", `@hourly`)

	SetDefaults()
	RegisterDockerFlags(cmd)
	RegisterSystemFlags(cmd)
	RegisterNotificationFlags(cmd)

	g.Expect(cmd.ParseFlags([]string{})).To(Succeed())
	flags := cmd.Flags()
	ProcessFlagAliases(flags)

	sched, _ := flags.GetString(`schedule`)
	g.Expect(sched).To(Equal(`@hourly`))
}

func TestProcessFlagAliasesInvalidPorcelaineVersion(t *testing.T) {
	g := NewWithT(t)
	logrus.StandardLogger().ExitFunc = func(_ int) { panic(`FATAL`) }
	cmd := new(cobra.Command)
	SetDefaults()
	RegisterDockerFlags(cmd)
	RegisterSystemFlags(cmd)
	RegisterNotificationFlags(cmd)

	g.Expect(cmd.ParseFlags([]string{`--porcelain`, `cowboy`})).To(Succeed())
	flags := cmd.Flags()

	g.Expect(func() {
		ProcessFlagAliases(flags)
	}).To(PanicWith(`FATAL`))
}

func TestFlagsArePrecentInDocumentation(t *testing.T) {
	// Legacy notifcations are ignored, since they are (soft) deprecated
	ignoredEnvs := map[string]string{
		"WATCHTOWER_NOTIFICATION_SLACK_ICON_EMOJI": "legacy",
		"WATCHTOWER_NOTIFICATION_SLACK_ICON_URL":   "legacy",
	}

	ignoredFlags := map[string]string{
		"notification-gotify-url":       "legacy",
		"notification-slack-icon-emoji": "legacy",
		"notification-slack-icon-url":   "legacy",
	}

	cmd := new(cobra.Command)
	SetDefaults()
	RegisterDockerFlags(cmd)
	RegisterSystemFlags(cmd)
	RegisterNotificationFlags(cmd)

	flags := cmd.PersistentFlags()

	docFiles := []string{
		"../../docs/arguments.md",
		"../../docs/lifecycle-hooks.md",
		"../../docs/notifications.md",
	}
	allDocs := ""
	var allDocsSb308 strings.Builder
	for _, f := range docFiles {
		bytes, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("Could not load docs file %q: %v", f, err)
		}
		allDocsSb308.WriteString(string(bytes))
	}
	allDocs += allDocsSb308.String()

	flags.VisitAll(func(f *pflag.Flag) {
		if !strings.Contains(allDocs, "--"+f.Name) {
			if _, found := ignoredFlags[f.Name]; !found {
				t.Logf("Docs does not mention flag long name %q", f.Name)
				t.Fail()
			}
		}
		if !strings.Contains(allDocs, "-"+f.Shorthand) {
			t.Logf("Docs does not mention flag shorthand %q (%q)", f.Shorthand, f.Name)
			t.Fail()
		}
	})

	for _, key := range AllEnvKeys() {
		envKey := strings.ToUpper(key)
		if !strings.Contains(allDocs, envKey) {
			if _, found := ignoredEnvs[envKey]; !found {
				t.Logf("Docs does not mention environment variable %q", envKey)
				t.Fail()
			}
		}
	}
}
