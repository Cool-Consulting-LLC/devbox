package cli

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
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

func TestRepoName(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:acme/api.git": "api",
		"https://github.com/acme/api": "api",
		"git@github.com:a/b.c.git":    "b.c",
		"nope":                        "",
	} {
		if got := RepoName(in); got != want {
			t.Errorf("RepoName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenameSecrets(t *testing.T) {
	refs := "OPENAI_API_KEY devbox-acme-api-OPENAI_API_KEY\nSHARED some-other-secret\n\nDB devbox-acme-api-DB\n"
	got, err := RenameSecrets("devbox", "acme-api", "acme", refs)
	if err != nil {
		t.Fatal(err)
	}
	want := []SecretMove{
		{"OPENAI_API_KEY", "devbox-acme-api-OPENAI_API_KEY", "devbox-acme-OPENAI_API_KEY"},
		{"DB", "devbox-acme-api-DB", "devbox-acme-DB"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("RenameSecrets = %+v", got)
	}
	// Dots are not allowed in secret IDs; both sides are mapped the same way.
	got, _ = RenameSecrets("devbox", "a.b", "c", "K devbox-a_b-K")
	if len(got) != 1 || got[0].To != "devbox-c-K" {
		t.Errorf("dotted: %+v", got)
	}
	if got, _ := RenameSecrets("devbox", "p", "q", ""); len(got) != 0 {
		t.Errorf("empty: %+v", got)
	}
}

func TestNewMachineKey(t *testing.T) {
	priv, pub, err := newMachineKey("devbox")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if pub.Type() != "ssh-ed25519" || !bytes.Equal(signer.PublicKey().Marshal(), pub.Marshal()) {
		t.Fatalf("public key %s does not match the private key", pub.Type())
	}
}

func TestMissingScopes(t *testing.T) {
	out := []byte("HTTP/2.0 200 OK\r\nX-Oauth-Scopes: gist, admin:public_key, repo\r\n\r\n{\"x-oauth-scopes\": \"\"}")
	if got := MissingScopes(out); len(got) != 1 || got[0] != "admin:ssh_signing_key" {
		t.Errorf("MissingScopes = %v", got)
	}
	if got := MissingScopes([]byte("HTTP/2.0 200 OK\r\n\r\n{}")); got != nil {
		t.Errorf("no scopes header: MissingScopes = %v", got)
	}
}
