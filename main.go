// Package main is the watchtower entry point. It delegates to the Cobra command tree in cmd.
package main

import (
	log "github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/cmd"
)

func init() {
	log.SetLevel(log.InfoLevel)
}

func main() {
	cmd.Execute()
}
