package config

import (
	"os"
	"strings"
	"testing"
)

const minimal = `
project_id: my-devbox-project
network: {name: devbox, subnet: devbox-europe-west4}
git: {name: A, email: a@example.com}
`

// noGit is minimal without git, for cases that set it.
const noGit = `
project_id: my-devbox-project
network: {name: devbox, subnet: devbox-europe-west4}
`

func TestExampleParses(t *testing.T) {
	data, err := os.ReadFile("../../devbox.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if c.Region() != "europe-west4" || c.SAEmail() != "devbox@my-devbox-project.iam.gserviceaccount.com" {
		t.Fatalf("derived values wrong: %s %s", c.Region(), c.SAEmail())
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "devbox" || c.MachineType != "e2-standard-4" || c.CodeDiskGB != 50 || !c.WantExternalIP() || !c.IdleEnabled() {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]string{
		"create_project":  "create_project: true\ngit: {name: A, email: a@b.c}",
		"both":            minimal + "create_project: true",
		"missing project": "git: {name: A, email: a@b.c}",
		"unknown key":     minimal + "colour: blue",
		"open internet":   minimal + "break_glass: {enabled: true, source_cidr: 0.0.0.0/0}",
		"empty cidr":      minimal + "break_glass: {enabled: true}",
		"wide cidr":       minimal + "break_glass: {enabled: true, source_cidr: 10.0.0.0/8}",
		"host bits":       minimal + "break_glass: {enabled: true, source_cidr: 1.2.3.4/24}",
		"bad secret id":   minimal + "tailscale: {oauth_secret: 'ts/oauth'}",
		"no network":      "project_id: my-devbox-project\ngit: {name: A, email: a@b.c}",
		"newline in name": noGit + "git: {name: \"A\\n[core]\\n\\tsshCommand = x\", email: a@b.c}",
		"space in email":  noGit + "git: {name: A, email: 'a b@c.d'}",
		"wildcard email":  noGit + "git: {name: A, email: '*@example.com'}",
		"list email":      noGit + "git: {name: A, email: 'a@example.com,b@example.com'}",
		"no-domain email": noGit + "git: {name: A, email: a@b}",
		"alert email":     minimal + "alerts: {email: 'nobody'}",
		"bad network":     "project_id: my-devbox-project\nnetwork: {name: 'x/../y', subnet: s}\ngit: {name: A, email: a@b.c}",
		"bad subnet":      "project_id: my-devbox-project\nnetwork: {name: n, subnet: 'S?x'}\ngit: {name: A, email: a@b.c}",
		"bad machine":     minimal + "machine_type: 'e2/../x'",
		"newline nested":  minimal + "extra_packages: [\"nodejs\\n\"]",
		"bad name":        minimal + "name: Dev_Box",
		"short name":      minimal + "name: dev",
		"snapshot time":   minimal + "snapshots: {start_time: '03:30'}",
		"idle vs high":    minimal + "alerts: {cpu_idle_pct: 90, cpu_high_pct: 80}",
		"bad zone":        minimal + "zone: frankfurt",
		"bad package":     minimal + "extra_packages: ['rm -rf /']",
		"bad sudo secret": minimal + "sudo: {password_secret: 'a/b'}",
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAccepts(t *testing.T) {
	for _, y := range []string{
		noGit + "git: {name: \"Ada Lovelace\", email: ada@example.com}",
		noGit + "git: {name: A, email: first.last+devbox@mail.example.co.uk}",
		minimal + "break_glass: {enabled: true, source_cidr: 203.0.113.7/32}",
		minimal + "break_glass: {enabled: true, source_cidr: '2001:db8::/64', allow_mosh: true}",
		minimal + "break_glass: {enabled: false, source_cidr: 0.0.0.0/0}", // ignored when disabled
		minimal + "external_ip: false\nidle_suspend: {enabled: false}",
		minimal + "extra_packages: [nodejs, python3-venv, golang-1.22]",
		minimal + "tailscale: {oauth_secret: ts-oauth}", // any secret ID: access is granted per secret
		minimal + "sudo: {password_secret: devbox-sudo-password-hash}",
	} {
		if _, err := Parse([]byte(y)); err != nil {
			t.Errorf("rejected %q: %v", y, err)
		}
	}
}

func TestDescribeCoversEveryKey(t *testing.T) {
	var b strings.Builder
	if err := WriteMarkdown(&b); err != nil {
		t.Fatal(err)
	}
	for _, k := range Keys() {
		if k.Describe == "" || k.Tier == "" {
			t.Errorf("%s: missing describe or tier tag", k.Path)
		}
		if _, ok := tierText[k.Tier]; !ok {
			t.Errorf("%s: unknown tier %q", k.Path, k.Tier)
		}
		if !strings.Contains(b.String(), "`"+k.Path+"`") {
			t.Errorf("%s missing from markdown", k.Path)
		}
	}
}

func TestGeneratedDocIsCurrent(t *testing.T) {
	var b strings.Builder
	_ = WriteMarkdown(&b)
	have, err := os.ReadFile("../../docs/config.md")
	if err != nil {
		t.Skip("docs/config.md not generated yet")
	}
	if string(have) != b.String() {
		t.Fatal("docs/config.md is stale: run `make docs`")
	}
}
