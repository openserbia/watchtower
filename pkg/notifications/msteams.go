package notifications

import (
	shoutrrrTeams "github.com/nicholas-fedor/shoutrrr/pkg/services/chat/teams"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	t "github.com/openserbia/watchtower/pkg/types"
)

const (
	msTeamsType = "msteams"
)

type msTeamsTypeNotifier struct {
	webHookURL string
	data       bool
}

func newMsTeamsNotifier(cmd *cobra.Command) t.ConvertibleNotifier {
	flags := cmd.Flags()

	webHookURL, _ := flags.GetString("notification-msteams-hook")
	if webHookURL == "" {
		log.Fatal("Required argument --notification-msteams-hook(cli) or WATCHTOWER_NOTIFICATION_MSTEAMS_HOOK_URL(env) is empty.")
	}

	withData, _ := flags.GetBool("notification-msteams-data")
	n := &msTeamsTypeNotifier{
		webHookURL: webHookURL,
		data:       withData,
	}

	return n
}

func (n *msTeamsTypeNotifier) GetURL(_ *cobra.Command) (string, error) {
	// shoutrrr v0.16 dropped the legacy Office 365 connector parser
	// (ConfigFromWebhookURL); the Teams service now only accepts Power Automate
	// workflow webhook URLs. Validate up front so a stale connector URL fails
	// at startup with a clear error instead of silently at send time.
	if err := shoutrrrTeams.ValidateWebhookURL(n.webHookURL); err != nil {
		return "", err
	}

	// The full workflow URL is carried verbatim in the `host` field; the new
	// Teams service has no hex theme color (Adaptive Cards use named enums), so
	// ColorHex is intentionally not forwarded here.
	config := &shoutrrrTeams.Config{
		Host: n.webHookURL,
	}

	return config.GetURL().String(), nil
}
