// Package remote runs commands on the box over SSH: Tailscale SSH by default,
// or through IAP when the tailnet is the thing that broke. On the box itself it
// skips SSH and runs the command directly.
package remote

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type Target struct {
	Host string // tailnet hostname; defaults to the box name
	User string // OS Login POSIX name
	IAP  bool   // use `gcloud compute ssh --tunnel-through-iap`
	// Local runs the command on this machine: we are on the box already, and
	// Tailscale SSH does not take connections from a node to itself.
	Local bool

	Project, Zone, Instance string
}

// Run executes cmd (a shell command line) on the box.
func (t Target) Run(ctx context.Context, cmd string, stdin io.Reader) error {
	var c *exec.Cmd
	if t.Local {
		c = exec.CommandContext(ctx, "sh", "-c", cmd)
	} else if t.IAP {
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
		// -- ends the options: a --host or --user starting with - is then a
		// (bad) destination, not an ssh option such as -oProxyCommand.
		c = exec.CommandContext(ctx, "ssh", append(args, "--", dest, cmd)...)
	}
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if stdin != nil {
		c.Stdin = stdin
	} else {
		c.Stdin = os.Stdin
	}
	if err := c.Run(); err != nil {
		how := "ssh " + t.Host
		if t.Local {
			how = "on this box"
		} else if t.IAP {
			how = "gcloud compute ssh --tunnel-through-iap"
		}
		return fmt.Errorf("%s: %w", how, err)
	}
	return nil
}

// WaitReachable retries a no-op SSH until it connects or timeout passes: a box
// just resumed takes a few seconds to be back on the tailnet.
func (t Target) WaitReachable(ctx context.Context, timeout time.Duration) error {
	if t.Local || t.IAP {
		return nil
	}
	dest := t.Host
	if t.User != "" {
		dest = t.User + "@" + t.Host
	}
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.CommandContext(ctx, "ssh", "-o", "ConnectTimeout=5", "-o", "BatchMode=yes", "--", dest, "true").CombinedOutput()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("ssh %s: %w: %s", t.Host, err, strings.TrimSpace(string(out)))
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
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
