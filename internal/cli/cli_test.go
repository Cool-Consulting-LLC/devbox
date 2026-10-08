package cli

import "testing"

func TestDirFor(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:acme/api.git":  "acme-api",
		"https://github.com/acme/api":  "acme-api",
		"https://github.com/acme/api/": "acme-api",
		"ssh://git@host/team/svc.git":  "team-svc",
		"git@github.com:a/b.c.git":     "a-b.c",
	} {
		if got := DirFor(in); got != want {
			t.Errorf("DirFor(%q) = %q, want %q", in, got, want)
		}
	}
}
