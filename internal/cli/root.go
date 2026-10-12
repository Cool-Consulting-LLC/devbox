// Package cli is the devbox command line. Each command's help text must agree
// with README.md; when they drift, the help text is wrong.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Cool-Consulting-LLC/devbox/internal/box"
	"github.com/Cool-Consulting-LLC/devbox/internal/config"
	"github.com/Cool-Consulting-LLC/devbox/internal/gcp"
)

var Version = "dev"

type globals struct {
	configPath string
	name       string
	codeDiskGB int64
}

func Execute() int {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "devbox:", err)
		if gcp.NeedsLogin(err) {
			fmt.Fprintln(os.Stderr, "devbox: your Google login expired; run `gcloud auth application-default login` and retry")
		}
		return 1
	}
	return 0
}

func newRoot() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:   "devbox",
		Short: "Stand up, pause and tear down a Claude Code workstation in your GCP project",
		Long: `devbox makes a GCP project match devbox.yaml: one instance reachable over your
tailnet, a pet disk at /srv/code, IAP and break-glass ways in, and alerts.
Stateless: resources are found by the label devbox=<name> and the prefix <name>-.

Credentials: Application Default Credentials (gcloud auth application-default login).
Docs: README.md (what it is, how to set it up, how to use it) and docs/config.md.`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&g.configPath, "config", "c", "devbox.yaml", "path to devbox.yaml")
	root.PersistentFlags().StringVar(&g.name, "name", "", "override name: address a different box, e.g. devbox-test")
	root.PersistentFlags().Int64Var(&g.codeDiskGB, "code-disk-gb", 0, "override code_disk_gb")

	root.AddCommand(
		preflightCmd(g), upCmd(g), downCmd(g), statusCmd(g), applyCmd(g),
		reprovisionCmd(g), projectCmd(g), destroyCmd(g), configCmd(),
	)
	return root
}

// load reads the config, applies flag overrides and connects to GCP.
func (g *globals) load(ctx context.Context) (*box.Box, error) {
	c, err := config.Load(g.configPath)
	if err != nil {
		return nil, err
	}
	if g.name != "" {
		c.Name = g.name
	}
	if g.codeDiskGB != 0 {
		c.CodeDiskGB = g.codeDiskGB
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	cl, err := gcp.New(ctx, c.ProjectID)
	if err != nil {
		return nil, err
	}
	return box.New(c, cl, os.Stdout), nil
}

func header(b *box.Box, verb string) {
	fmt.Printf("%s %s in %s (%s)\n", verb, b.C.Name, b.C.ProjectID, b.C.Zone)
}

// confirm asks for an exact word on the terminal. Refuses when stdin is not a TTY.
func confirm(prompt, want string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("refusing without a terminal to confirm on; pass --yes")
	}
	fmt.Printf("%s Type %q to continue: ", prompt, want)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(line) != want {
		return errors.New("not confirmed; nothing changed")
	}
	return nil
}
