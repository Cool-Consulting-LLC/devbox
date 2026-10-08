package assets

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Cool-Consulting-LLC/devbox/internal/config"
)

func TestCloudInitIsValidYAML(t *testing.T) {
	ci := CloudInit()
	if !strings.HasPrefix(ci, "#cloud-config\n") {
		t.Fatal("missing #cloud-config header")
	}
	var doc struct {
		Packages   []string `yaml:"packages"`
		WriteFiles []struct {
			Path, Content, Permissions string
		} `yaml:"write_files"`
		Runcmd []any `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(ci), &doc); err != nil {
		t.Fatal(err)
	}
	byPath := map[string]string{}
	for _, f := range doc.WriteFiles {
		byPath[f.Path] = f.Content
	}
	// Embedded files must round-trip byte for byte through the YAML block scalars.
	for path, src := range map[string]string{
		"/etc/claude-code/managed-settings.json": "managed-settings.json",
		"/etc/claude-code/hooks/guard.sh":        "guard.sh",
		"/usr/local/sbin/devbox-bootstrap":       "bootstrap.sh",
	} {
		if byPath[path] != read(src) {
			t.Errorf("%s does not round-trip through cloud-init", path)
		}
	}
	var ms map[string]any
	if err := json.Unmarshal([]byte(byPath["/etc/claude-code/managed-settings.json"]), &ms); err != nil {
		t.Errorf("managed-settings.json: %v", err)
	}
	if len(ci) > 200*1024 {
		t.Errorf("user-data is %d bytes; metadata values are capped at 256 KB", len(ci))
	}
}

func TestShellScriptsParse(t *testing.T) {
	dir := t.TempDir()
	scripts := map[string]string{"provision.sh": Provision(), "bootstrap.sh": read("bootstrap.sh"), "guard.sh": read("guard.sh")}
	for _, f := range boxFiles {
		if strings.HasSuffix(f.mode, "755") && strings.HasPrefix(read(f.src), "#!/bin/bash") {
			scripts[filepath.Base(f.dst)] = read(f.src)
		}
	}
	for name, body := range scripts {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("bash", "-n", p).CombinedOutput(); err != nil {
			t.Errorf("%s: %s", name, out)
		}
		if sc, err := exec.LookPath("shellcheck"); err == nil {
			if out, err := exec.Command(sc, "-S", "warning", "-s", "bash", p).CombinedOutput(); err != nil {
				t.Errorf("shellcheck %s:\n%s", name, out)
			}
		}
	}
	if len(Provision()) > 200*1024 {
		t.Error("provision.sh too large for one metadata value")
	}
	if strings.Contains(Provision(), "@@WRITE_FILES@@") {
		t.Error("write_files marker not replaced")
	}
}

func TestPythonCompiles(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	for _, f := range boxFiles {
		body := read(f.src)
		if !strings.HasPrefix(body, "#!/usr/bin/python3") {
			continue
		}
		c := exec.Command("python3", "-c", "import ast,sys; ast.parse(sys.stdin.read())")
		c.Stdin = strings.NewReader(body)
		if out, err := c.CombinedOutput(); err != nil {
			t.Errorf("%s: %s", f.src, out)
		}
	}
}

func TestGuardHook(t *testing.T) {
	p := filepath.Join(t.TempDir(), "guard.sh")
	if err := os.WriteFile(p, []byte(GuardHook()), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	deny := []string{
		"git push", "git  push origin main", `eval "git push"`, "sh -c 'git push'",
		"cd x && git commit -m hi", "git -C /srv/code/x push", "git -c user.name=x commit",
		"/usr/bin/git push", "terraform -chdir=infra apply", "terraform apply -auto-approve",
		"devbox destroy --all", "sudo git push", "echo $(git push)", "ls | xargs git commit",
		"git --no-pager push", "true;git push",
	}
	allow := []string{
		"git status", "git log --oneline", "terraform plan", "git commit-tree", "grep pushd file",
		"echo git push is bad", "git pushx", "devbox status", "terraform apply-something",
	}
	run := func(cmd string) bool {
		in, _ := json.Marshal(map[string]any{"tool_input": map[string]string{"command": cmd}})
		c := exec.Command(p)
		c.Stdin = strings.NewReader(string(in))
		out, err := c.Output()
		if err != nil {
			t.Fatalf("%q: %v", cmd, err)
		}
		return strings.Contains(string(out), `"deny"`)
	}
	for _, c := range deny {
		if !run(c) {
			t.Errorf("not denied: %s", c)
		}
	}
	for _, c := range allow {
		if run(c) {
			t.Errorf("wrongly denied: %s", c)
		}
	}
}

func TestConfigEnvQuotes(t *testing.T) {
	c := config.Defaults()
	c.ProjectID = "p-123456"
	c.Git.Name = "O'Brien; rm -rf /"
	c.Git.Email = "o@example.com"
	env := ConfigEnv(c)
	out, err := exec.Command("bash", "-c", env+"\nprintf %s \"$DEVBOX_GIT_NAME\"").Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != c.Git.Name {
		t.Fatalf("got %q", out)
	}
}
