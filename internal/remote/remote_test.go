package remote

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestJoinRoundTrips(t *testing.T) {
	words := []string{"devbox-project", "add", "git@github.com:acme/api.git", "acme-api", "K=it's $HOME `x`", "A=b c"}
	out, err := exec.Command("bash", "-c", `for w in `+Join(words...)+`; do printf '%s\n' "$w"; done`).Output()
	if err != nil {
		t.Fatal(err)
	}
	want := ""
	for _, w := range words {
		want += w + "\n"
	}
	if string(out) != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
}

func TestLocalRunFeedsStdin(t *testing.T) {
	f := t.TempDir() + "/out"
	err := Target{Local: true}.Run(context.Background(), "cat > "+Quote(f), strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f); string(b) != "hello" {
		t.Fatalf("got %q", b)
	}
}

func TestParseTailnetPeer(t *testing.T) {
	const status = `{"BackendState":"Running","Self":{"DNSName":"laptop.tail1.ts.net.","Online":true},
	"Peer":{"nodekey:a":{"HostName":"devbox","DNSName":"devbox.tail1.ts.net.","Online":true,"TailscaleIPs":["fd7a::1","100.1.2.3"]},
	        "nodekey:b":{"HostName":"other","DNSName":"other.tail1.ts.net.","Online":false,"TailscaleIPs":["100.4.5.6"]}}}`
	ip, err := parseTailnetPeer([]byte(status), "devbox")
	if err != nil || ip != "100.1.2.3" {
		t.Fatalf("got %q, %v", ip, err)
	}
	if _, err := parseTailnetPeer([]byte(status), "Other"); !errors.Is(err, ErrTailnetTransient) || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("offline peer: %v", err)
	}
	if _, err := parseTailnetPeer([]byte(status), "nope"); !errors.Is(err, ErrTailnetTransient) || !strings.Contains(err.Error(), "not in your tailnet") {
		t.Fatalf("missing peer: %v", err)
	}
	stopped := `{"BackendState":"NeedsLogin","Peer":null}`
	if _, err := parseTailnetPeer([]byte(stopped), "devbox"); err == nil || errors.Is(err, ErrTailnetTransient) || !strings.Contains(err.Error(), "NeedsLogin") {
		t.Fatalf("local backend down: %v", err)
	}
}
