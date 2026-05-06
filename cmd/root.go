// Package cmd contains the watchtower (sub-)commands.
package cmd

import (
	"context"
	"errors"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/openserbia/watchtower/internal/actions"
	"github.com/openserbia/watchtower/internal/events"
	"github.com/openserbia/watchtower/internal/flags"
	"github.com/openserbia/watchtower/internal/meta"
	"github.com/openserbia/watchtower/pkg/api"
	apiAudit "github.com/openserbia/watchtower/pkg/api/audit"
	apiMetrics "github.com/openserbia/watchtower/pkg/api/metrics"
	"github.com/openserbia/watchtower/pkg/api/update"
	"github.com/openserbia/watchtower/pkg/container"
	"github.com/openserbia/watchtower/pkg/filters"
	"github.com/openserbia/watchtower/pkg/metrics"
	"github.com/openserbia/watchtower/pkg/notifications"
	"github.com/openserbia/watchtower/pkg/registry/transport"
	t "github.com/openserbia/watchtower/pkg/types"
)

var (
	client             container.Client
	scheduleSpec       string
	cleanup            bool
	noRestart          bool
	noPull             bool
	monitorOnly        bool
	enableLabel        bool
	auditUnmanaged     bool
	healthCheckGated   bool
	healthCheckTimeout time.Duration
	imageCooldown      time.Duration
	disableContainers  []string
	notifier           t.Notifier
	timeout            time.Duration
	lifecycleHooks     bool
	rollingRestart     bool
	composeDependsOn   bool
	scope              string
	labelPrecedence    bool
	runOnce            bool
	selfContainerID    t.ContainerID
)

var rootCmd = NewRootCommand()

// NewRootCommand creates the root command for watchtower
func NewRootCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "watchtower",
		Short: "Automatically updates running Docker containers",
		Long: `
	Watchtower automatically updates running Docker containers whenever a new image is released.
	More information available at https://github.com/openserbia/watchtower/.
	`,
		Version: meta.Version,
		Run:     Run,
		PreRun:  PreRun,
		Args:    cobra.ArbitraryArgs,
	}
}

func init() {
	flags.SetDefaults()
	flags.RegisterDockerFlags(rootCmd)
	flags.RegisterSystemFlags(rootCmd)
	flags.RegisterNotificationFlags(rootCmd)
}

// Execute the root func and exit in case of errors
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}

// PreRun is a lifecycle hook that runs before the command is executed.
func PreRun(cmd *cobra.Command, _ []string) {
	f := cmd.PersistentFlags()
	flags.ProcessFlagAliases(f)
	if err := flags.SetupLogging(f); err != nil {
		log.Fatalf("Failed to initialize logging: %s", err.Error())
	}

	scheduleSpec, _ = f.GetString("schedule")

	flags.GetSecretsFromFiles(cmd)
	cleanup, noRestart, monitorOnly, timeout = flags.ReadFlags(cmd)

	if timeout < 0 {
		log.Fatal("Please specify a positive value for timeout value.")
	}

	enableLabel, _ = f.GetBool("label-enable")
	auditUnmanaged, _ = f.GetBool("audit-unmanaged")
	healthCheckGated, _ = f.GetBool("health-check-gated")
	healthCheckTimeout, _ = f.GetDuration("health-check-timeout")
	imageCooldown, _ = f.GetDuration("image-cooldown")
	disableContainers, _ = f.GetStringSlice("disable-containers")

	insecureRegistries, _ := f.GetStringSlice("insecure-registry")
	caBundle, _ := f.GetString("registry-ca-bundle")
	if err := transport.Configure(insecureRegistries, caBundle); err != nil {
		log.Fatalf("Failed to configure registry transport: %s", err)
	}
	lifecycleHooks, _ = f.GetBool("enable-lifecycle-hooks")
	rollingRestart, _ = f.GetBool("rolling-restart")
	composeDependsOn, _ = f.GetBool("compose-depends-on")
	scope, _ = f.GetString("scope")
	labelPrecedence, _ = f.GetBool("label-take-precedence")

	if scope != "" {
		log.Debugf(`Using scope %q`, scope)
	}

	// configure environment vars for client
	err := flags.EnvConfig(cmd)
	if err != nil {
		log.Fatal(err)
	}

	noPull, _ = f.GetBool("no-pull")
	includeStopped, _ := f.GetBool("include-stopped")
	includeRestarting, _ := f.GetBool("include-restarting")
	reviveStopped, _ := f.GetBool("revive-stopped")
	removeVolumes, _ := f.GetBool("remove-volumes")
	disableMemorySwappiness, _ := f.GetBool("disable-memory-swappiness")
	warnOnHeadPullFailed, _ := f.GetString("warn-on-head-failure")

	if monitorOnly && noPull {
		log.Warn("Using `WATCHTOWER_NO_PULL` and `WATCHTOWER_MONITOR_ONLY` simultaneously might lead to no action being taken at all. If this is intentional, you may safely ignore this message.")
	}

	client = container.NewClient(container.ClientOptions{
		IncludeStopped:          includeStopped,
		ReviveStopped:           reviveStopped,
		RemoveVolumes:           removeVolumes,
		IncludeRestarting:       includeRestarting,
		DisableMemorySwappiness: disableMemorySwappiness,
		WarnOnHeadFailed:        container.WarningStrategy(warnOnHeadPullFailed),
	})

	notifier = notifications.NewNotifier(cmd)
	notifier.AddLogHook()
}

// Run is the main execution flow of the command
func Run(c *cobra.Command, names []string) {
	filter, filterDesc := filters.BuildFilter(names, disableContainers, enableLabel, scope)
	runOnce, _ = c.PersistentFlags().GetBool("run-once")
	enableUpdateAPI, _ := c.PersistentFlags().GetBool("http-api-update")
	enableMetricsAPI, _ := c.PersistentFlags().GetBool("http-api-metrics")
	unblockHTTPAPI, _ := c.PersistentFlags().GetBool("http-api-periodic-polls")
	apiToken, _ := c.PersistentFlags().GetString("http-api-token")
	healthCheck, _ := c.PersistentFlags().GetBool("health-check")

	if healthCheck {
		// health check should not have pid 1
		if os.Getpid() == 1 {
			time.Sleep(1 * time.Second)
			log.Fatal("The health check flag should never be passed to the main watchtower container process")
		}
		os.Exit(0)
	}

	if rollingRestart && monitorOnly {
		log.Fatal("Rolling restarts is not compatible with the global monitor only flag")
	}

	if rollingRestart && composeDependsOn {
		log.Warn("--rolling-restart is typically incompatible with --compose-depends-on: rolling restarts update containers one at a time without coordinating dependency chains, so a depends_on graph won't be respected. Pick one of the two for Compose stacks with meaningful dependencies.")
	}

	awaitDockerClient()

	// Detect our own container ID once after the daemon is reachable, so
	// restartStaleContainer can distinguish the actual running self (rename-
	// and-respawn) from orphan watchtower-labeled containers (stop+remove,
	// no respawn). Empty result falls back to the IsWatchtower label check.
	selfContainerID = actions.DetectSelfContainerID(client)

	if err := actions.CheckForSanity(client, filter, rollingRestart); err != nil {
		logNotifyExit(err)
	}

	if runOnce {
		writeStartupMessage(c, time.Time{}, filterDesc)
		runUpdatesWithNotifications(filter)
		notifier.Close()
		os.Exit(0)
		return
	}

	if err := actions.CheckForMultipleWatchtowerInstances(client, cleanup, scope); err != nil {
		logNotifyExit(err)
	}

	// The lock is shared between the scheduler and the HTTP API. It only allows one update to run at a time.
	updateLock := make(chan bool, 1)
	updateLock <- true

	httpAPI := api.New(apiToken)
	if addr, _ := c.PersistentFlags().GetString("http-api-host"); addr != "" {
		httpAPI.ListenAddr = addr
	}

	if enableUpdateAPI {
		updateHandler := update.New(func(images []string) update.Response {
			metric := runUpdatesWithNotifications(filters.FilterByImage(images, filter))
			metrics.RegisterScan(metric)
			return update.Response{
				Status:  "completed",
				Scanned: metric.Scanned,
				Updated: metric.Updated,
				Failed:  metric.Failed,
			}
		}, updateLock)
		httpAPI.RegisterFunc(updateHandler.Path, updateHandler.Handle)
		// If polling isn't enabled the scheduler is never started, and
		// we need to trigger the startup messages manually.
		if !unblockHTTPAPI {
			writeStartupMessage(c, time.Time{}, filterDesc)
		}
	}

	if enableMetricsAPI {
		metricsHandler := apiMetrics.New()
		metricsNoAuth, _ := c.PersistentFlags().GetBool("http-api-metrics-no-auth")
		if metricsNoAuth {
			log.Debug("Serving /v1/metrics without token auth — ensure :8080 is on a trusted network, bound to localhost, or fronted by a reverse proxy.")
			httpAPI.RegisterPublicHandler(metricsHandler.Path, metricsHandler.Handle)
		} else {
			httpAPI.RegisterHandler(metricsHandler.Path, metricsHandler.Handle)
		}
	}

	enableAuditAPI, _ := c.PersistentFlags().GetBool("http-api-audit")
	if enableAuditAPI {
		auditHandler := apiAudit.New(client, scope)
		httpAPI.RegisterFunc(auditHandler.Path, auditHandler.Handle)
	}

	if err := httpAPI.Start(enableUpdateAPI && !unblockHTTPAPI); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("failed to start API", err)
	}

	eventCtx, cancelEvents := context.WithCancel(context.Background())
	if watchEvents, _ := c.PersistentFlags().GetBool("watch-docker-events"); watchEvents {
		startEventWatcher(eventCtx, filter, updateLock)
	}

	if err := runUpgradesOnSchedule(c, filter, filterDesc, updateLock); err != nil {
		log.Error(err)
	}

	// os.Exit skips deferred calls, so tear the event watcher down explicitly
	// after the scheduler returns (either from signal or error).
	cancelEvents()
	os.Exit(1)
}

// startEventWatcher launches a goroutine that streams image tag/load events
// from the Docker engine and triggers a targeted scan for local-image rebuilds.
// The watcher shares updateLock with the scheduler so event-driven and
// scheduled scans never run concurrently. Event-driven scans are best-effort:
// if the lock is held (an update is already running), we drop the trigger on
// the floor — the poll loop is the safety net.
func startEventWatcher(ctx context.Context, filter t.Filter, lock chan bool) {
	watcher := events.NewWatcher(client, events.Config{
		Trigger: func(imageNames []string) {
			select {
			case v := <-lock:
				defer func() { lock <- v }()
				targeted := filters.FilterByImage(imageNames, filter)
				metric := runUpdatesWithNotifications(targeted)
				metrics.RegisterScan(metric)
			default:
				log.WithField("images", imageNames).
					Debug("Event-triggered scan skipped: another update is in progress")
			}
		},
	})
	go watcher.Run(ctx)
	log.Info("Watching Docker engine for image tag/load events")
}

func logNotifyExit(err error) {
	log.Error(err)
	notifier.Close()
	os.Exit(1)
}

func awaitDockerClient() {
	log.Debug("Sleeping for a second to ensure the docker api client has been properly initialized.")
	time.Sleep(1 * time.Second)
}

const secondsPerHour = 60

func formatDuration(d time.Duration) string {
	sb := strings.Builder{}

	hours := int64(d.Hours())
	minutes := int64(math.Mod(d.Minutes(), secondsPerHour))
	seconds := int64(math.Mod(d.Seconds(), secondsPerHour))

	if hours == 1 {
		sb.WriteString("1 hour")
	} else if hours != 0 {
		sb.WriteString(strconv.FormatInt(hours, 10))
		sb.WriteString(" hours")
	}

	if hours != 0 && (seconds != 0 || minutes != 0) {
		sb.WriteString(", ")
	}

	if minutes == 1 {
		sb.WriteString("1 minute")
	} else if minutes != 0 {
		sb.WriteString(strconv.FormatInt(minutes, 10))
		sb.WriteString(" minutes")
	}

	if minutes != 0 && (seconds != 0) {
		sb.WriteString(", ")
	}

	if seconds == 1 {
		sb.WriteString("1 second")
	} else if seconds != 0 || (hours == 0 && minutes == 0) {
		sb.WriteString(strconv.FormatInt(seconds, 10))
		sb.WriteString(" seconds")
	}

	return sb.String()
}

func writeStartupMessage(c *cobra.Command, sched time.Time, filtering string) {
	noStartupMessage, _ := c.PersistentFlags().GetBool("no-startup-message")
	enableUpdateAPI, _ := c.PersistentFlags().GetBool("http-api-update")

	var startupLog *log.Entry
	if noStartupMessage {
		startupLog = notifications.LocalLog
	} else {
		startupLog = log.NewEntry(log.StandardLogger())
		// Batch up startup messages to send them as a single notification
		notifier.StartNotification()
	}

	startupLog.Info("Watchtower ", meta.Version)

	notifierNames := notifier.GetNames()
	if len(notifierNames) > 0 {
		startupLog.Info("Using notifications: " + strings.Join(notifierNames, ", "))
	} else {
		startupLog.Info("Using no notifications")
	}

	startupLog.Info(filtering)

	if !sched.IsZero() {
		until := formatDuration(time.Until(sched))
		startupLog.Info("Scheduling first run: " + sched.Format("2006-01-02 15:04:05 -0700 MST"))
		startupLog.Info("Note that the first check will be performed in " + until)
	} else if runOnce, _ := c.PersistentFlags().GetBool("run-once"); runOnce {
		startupLog.Info("Running a one time update.")
	} else {
		startupLog.Info("Periodic runs are not enabled.")
	}

	if enableUpdateAPI {
		listenAddr, _ := c.PersistentFlags().GetString("http-api-host")
		if listenAddr == "" {
			listenAddr = api.DefaultListenAddr
		}
		startupLog.Debug("The HTTP API is enabled at " + listenAddr + ".")
	}

	if !noStartupMessage {
		// Send the queued up startup messages, not including the trace warning below (to make sure it's noticed)
		notifier.SendNotification(nil)
	}

	if log.IsLevelEnabled(log.TraceLevel) {
		startupLog.Warn("Trace level enabled: log will include sensitive information as credentials and tokens")
	}
}

func runUpgradesOnSchedule(c *cobra.Command, filter t.Filter, filtering string, lock chan bool) error {
	if lock == nil {
		lock = make(chan bool, 1)
		lock <- true
	}

	// cron/v3 switched to the standard 5-field crontab spec by default.
	// Watchtower has historically exposed a 6-field spec (with a leading seconds field),
	// so opt back in via WithSeconds to keep existing WATCHTOWER_SCHEDULE values working.
	scheduler := cron.New(cron.WithSeconds())
	_, err := scheduler.AddFunc(
		scheduleSpec,
		func() {
			select {
			case v := <-lock:
				defer func() { lock <- v }()
				metric := runUpdatesWithNotifications(filter)
				metrics.RegisterScan(metric)
			default:
				// Update was skipped
				metrics.RegisterScan(nil)
				log.Debug("Skipped another update already running.")
			}

			nextRuns := scheduler.Entries()
			if len(nextRuns) > 0 {
				log.Debug("Scheduled next run: " + nextRuns[0].Next.String())
			}
		})
	if err != nil {
		return err
	}

	schedule := scheduler.Entries()[0].Schedule
	firstFire := schedule.Next(time.Now())
	// Derive the cadence between two consecutive fires. For fixed interval
	// schedules this is exact; for irregular cron expressions it's a best-effort
	// approximation of "typical gap" — good enough for the staleness alert.
	metrics.SetPollInterval(schedule.Next(firstFire).Sub(firstFire))

	writeStartupMessage(c, firstFire, filtering)

	// Opt-in: fire one scan right away so operators can verify a fresh
	// deployment without waiting for the first scheduled tick. Skipped if
	// the HTTP API would race us for the lock.
	if updateOnStart, _ := c.PersistentFlags().GetBool("update-on-start"); updateOnStart {
		log.Info("--update-on-start: running an initial scan before the scheduler begins")
		select {
		case v := <-lock:
			// Release inside a closure so a panic in runUpdatesWithNotifications
			// doesn't wedge both the scheduler and the HTTP API. Matches the
			// pattern used by the scheduler callback below.
			func() {
				defer func() { lock <- v }()
				metric := runUpdatesWithNotifications(filter)
				metrics.RegisterScan(metric)
			}()
		default:
			log.Debug("Skipped initial scan: another update is already holding the lock")
		}
	}

	scheduler.Start()

	// Graceful shut-down on SIGINT/SIGTERM
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	signal.Notify(interrupt, syscall.SIGTERM)

	<-interrupt
	scheduler.Stop()
	log.Info("Waiting for running update to be finished...")
	<-lock
	return nil
}

func runUpdatesWithNotifications(filter t.Filter) *metrics.Metric {
	notifier.StartNotification()
	// Always-on: classify containers and publish managed/excluded/unmanaged
	// gauges so the Grafana dashboard has data to show. Log warnings are
	// opt-in via --audit-unmanaged (only meaningful under --label-enable).
	if err := actions.AuditUnmanaged(client, scope, auditUnmanaged && enableLabel); err != nil {
		log.WithError(err).Warn("Failed to audit container watch status")
	}
	updateParams := t.UpdateParams{
		Filter:             filter,
		Cleanup:            cleanup,
		NoRestart:          noRestart,
		Timeout:            timeout,
		MonitorOnly:        monitorOnly,
		LifecycleHooks:     lifecycleHooks,
		RollingRestart:     rollingRestart,
		LabelPrecedence:    labelPrecedence,
		NoPull:             noPull,
		HealthCheckGated:   healthCheckGated,
		HealthCheckTimeout: healthCheckTimeout,
		ImageCooldown:      imageCooldown,
		ComposeDependsOn:   composeDependsOn,
		RunOnce:            runOnce,
		SelfContainerID:    selfContainerID,
	}
	result, err := actions.Update(client, updateParams)
	if err != nil {
		log.Error(err)
	}
	notifier.SendNotification(result)
	metricResults := metrics.NewMetric(result)
	notifications.LocalLog.WithFields(log.Fields{
		"Scanned": metricResults.Scanned,
		"Updated": metricResults.Updated,
		"Failed":  metricResults.Failed,
	}).Info("Session done")
	return metricResults
}
