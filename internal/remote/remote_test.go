package remote

import (
	"context"
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
