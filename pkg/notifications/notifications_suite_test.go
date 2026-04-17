package notifications_test

import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
)

func TestNotifications(t *testing.T) {
	RegisterFailHandler(Fail)
	format.CharactersAroundMismatchToInclude = 20
	RunSpecs(t, "Notifications Suite")
}
