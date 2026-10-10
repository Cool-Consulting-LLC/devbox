package cli

import (
	"strings"
	"testing"
)

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

func TestGcloudEnv(t *testing.T) {
	got, err := GcloudEnv("client-a")
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "CLOUDSDK_CONFIG=/srv/code/.gcloud/client-a" {
		t.Errorf("GcloudEnv(client-a)[0] = %q", got[0])
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b", "-x", ".hidden", "a b"} {
		if _, err := GcloudEnv(bad); err == nil {
			t.Errorf("GcloudEnv(%q) accepted", bad)
		}
	}
}

func TestUnsetWords(t *testing.T) {
	got, err := UnsetWords([]string{"region", "FOO"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "-CLOUDSDK_COMPUTE_REGION -FOO" {
		t.Errorf("UnsetWords = %q", got)
	}
	for _, bad := range []string{"", "-FOO", "A=B", "1X"} {
		if _, err := UnsetWords([]string{bad}); err == nil {
			t.Errorf("UnsetWords(%q) accepted", bad)
		}
	}
}

func TestEnvFlagsKV(t *testing.T) {
	f := envFlags{gcloudConfig: "c", region: "r", env: []string{"A=b=c"}}
	got, err := f.kv()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[3] != "CLOUDSDK_COMPUTE_REGION=r" || got[4] != "A=b=c" {
		t.Errorf("kv = %q", got)
	}
	for _, bad := range []envFlags{{env: []string{"NOEQ"}}, {env: []string{"K=@secret:x"}}, {gcloudConfig: "../x"}} {
		if _, err := bad.kv(); err == nil {
			t.Errorf("kv(%+v) accepted", bad)
		}
	}
}
