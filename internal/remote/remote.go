// Package remote runs commands on the box over SSH: Tailscale SSH by default,
// or through IAP when the tailnet is the thing that broke.
package remote

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

type Target struct {
	Host string // tailnet hostname; defaults to the box name
	User string // OS Login POSIX name
	IAP  bool   // use `gcloud compute ssh --tunnel-through-iap`

	Project, Zone, Instance string
}

// Run executes cmd (a shell command line) on the box.
func (t Target) Run(ctx context.Context, cmd string, stdin io.Reader) error {
	var c *exec.Cmd
	if t.IAP {
		args := []string{"compute", "ssh", t.Instance, "--project", t.Project, "--zone", t.Zone, "--tunnel-through-iap", "--command", cmd}
		c = exec.CommandContext(ctx, "gcloud", args...)
	} else {
		dest := t.Host
		if t.User != "" {
			dest = t.User + "@" + t.Host
		}
		args := []string{"-o", "ServerAliveInterval=15"}
		if stdin != nil {
			args = append(args, "-T")
		}
		c = exec.CommandContext(ctx, "ssh", append(args, dest, cmd)...)
	}
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if stdin != nil {
		c.Stdin = stdin
	} else {
		c.Stdin = os.Stdin
	}
	if err := c.Run(); err != nil {
		how := "ssh " + t.Host
		if t.IAP {
			how = "gcloud compute ssh --tunnel-through-iap"
		}
		return fmt.Errorf("%s: %w", how, err)
	}
	return nil
}

var safeWord = regexp.MustCompile(`^[A-Za-z0-9_./=:@%+,-]+$`)

// Quote makes s a single shell word.
func Quote(s string) string {
	if safeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Join quotes each word and joins them into one command line.
func Join(words ...string) string {
	q := make([]string, len(words))
	for i, w := range words {
		q[i] = Quote(w)
	}
	return strings.Join(q, " ")
}
