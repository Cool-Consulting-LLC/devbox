// Command devbox stands up a Claude Code workstation in a GCP project you own.
package main

import (
	"os"

	"github.com/Cool-Consulting-LLC/devbox/internal/cli"
)

func main() { os.Exit(cli.Execute()) }
