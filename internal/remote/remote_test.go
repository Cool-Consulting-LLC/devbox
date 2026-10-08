package remote

import (
	"os/exec"
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
