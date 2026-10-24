package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrTailnetTransient marks a tailnet state that a few seconds may fix: the
// box just resumed and is not back yet. The others need a person.
var ErrTailnetTransient = errors.New("transient")

// TailnetPeer asks this machine's tailscale CLI whether host is an online node
// of its tailnet, and returns the node's IPv4 address. It fails when the CLI is
// missing, when this machine is not on the tailnet, and when the box is not on
// it or offline; the last two wrap ErrTailnetTransient.
func TailnetPeer(ctx context.Context, host string) (string, error) {
	if _, err := exec.LookPath("tailscale"); err != nil {
		return "", errors.New("no tailscale CLI on this machine; install it to reach the box by name")
	}
	out, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			err = errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("tailscale status: %w", err)
	}
	return parseTailnetPeer(out, host)
}

type tsStatus struct {
	BackendState string
	Peer         map[string]tsNode
}

type tsNode struct {
	DNSName      string
	Online       bool
	TailscaleIPs []string
}

func parseTailnetPeer(status []byte, host string) (string, error) {
	var st tsStatus
	if err := json.Unmarshal(status, &st); err != nil {
		return "", fmt.Errorf("tailscale status: %w", err)
	}
	if st.BackendState != "Running" {
		return "", fmt.Errorf("tailscale is %s on this machine; run `tailscale up`", st.BackendState)
	}
	for _, p := range st.Peer {
		// The first label of the MagicDNS name is what `ssh host` resolves. The
		// hostname can differ when a name clash made the node host-1.
		name, _, _ := strings.Cut(p.DNSName, ".")
		if !strings.EqualFold(name, host) {
			continue
		}
		if !p.Online {
			return "", fmt.Errorf("%w: %s is in your tailnet but offline", ErrTailnetTransient, host)
		}
		for _, ip := range p.TailscaleIPs {
			if !strings.Contains(ip, ":") {
				return ip, nil
			}
		}
		return "", nil
	}
	return "", fmt.Errorf("%w: %s is not in your tailnet", ErrTailnetTransient, host)
}
