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
		"/usr/local/sbin/devbox-bootstrap": "bootstrap.sh",
	} {
		if byPath[path] != read(src) {
			t.Errorf("%s does not round-trip through cloud-init", path)
		}
	}
	// Claude Code policy is reprovision tier; it must not have crept back.
	for path := range byPath {
		if strings.HasPrefix(path, "/etc/claude-code/") {
			t.Errorf("%s belongs in boxFiles, not cloud-init", path)
		}
	}
	if len(ci) > 200*1024 {
		t.Errorf("user-data is %d bytes; metadata values are capped at 256 KB", len(ci))
	}
}

func TestShellScriptsParse(t *testing.T) {
	dir := t.TempDir()
	scripts := map[string]string{"provision.sh": Provision(), "bootstrap.sh": read("bootstrap.sh")}
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

func TestManagedSettings(t *testing.T) {
	var ms struct {
		StatusLine struct{ Command string } `json:"statusLine"`
		Hooks      map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	if err := json.Unmarshal([]byte(read("box/managed-settings.json")), &ms); err != nil {
		t.Fatal(err)
	}
	// Every path the settings name must be a file provision.sh writes.
	dsts := map[string]bool{}
	for _, f := range boxFiles {
		dsts[f.dst] = true
	}
	want := []string{ms.StatusLine.Command}
	for _, hs := range ms.Hooks {
		for _, h := range hs {
			for _, c := range h.Hooks {
				want = append(want, c.Command)
			}
		}
	}
	for _, p := range want {
		if !dsts[p] {
			t.Errorf("managed-settings.json names %s, which no boxFile writes", p)
		}
	}
	// Reprovision rewrites it through a quoted heredoc; the JSON must survive.
	prov := Provision()
	if !strings.Contains(prov, read("box/managed-settings.json")) {
		t.Error("managed-settings.json is not carried verbatim by provision.sh")
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

func TestStatusLine(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "devbox-claude-statusline")
	if err := os.WriteFile(p, []byte(read("box/devbox-claude-statusline")), 0o755); err != nil {
		t.Fatal(err)
	}
	gc := filepath.Join(dir, "gcloud")
	if err := os.MkdirAll(gc, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(stdin string, env ...string) string {
		c := exec.Command(p)
		c.Stdin = strings.NewReader(stdin)
		c.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "CLOUDSDK_CONFIG=" + gc}, env...)
		out, err := c.Output()
		if err != nil {
			t.Fatalf("%q: %v", stdin, err)
		}
		return string(out)
	}
	// Nothing known: still a line, never an error.
	if out := run("{}"); !strings.Contains(out, "no ADC") {
		t.Errorf("empty input: %q", out)
	}
	// A project session with ADC: identity, meters, model.
	if err := os.WriteFile(filepath.Join(gc, "application_default_credentials.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := `{"model":{"display_name":"Opus"},"workspace":{"current_dir":"` + dir + `","project_dir":"` + dir + `"},` +
		`"context_window":{"used_percentage":31.4},"rate_limits":{"five_hour":{"used_percentage":12}},` +
		`"cost":{"total_cost_usd":0.42,"total_duration_ms":4521000,"total_lines_added":8,"total_lines_removed":1}}`
	out := run(in, "CLOUDSDK_CORE_PROJECT=acme-prod", "CLOUDSDK_COMPUTE_REGION=europe-west2")
	for _, want := range []string{"gcloud@acme-prod/europe-west2", "✓", "31%\x1b[0m \x1b[32m12%\x1b[0m  ", "$0.42 1h15m +8/-1", "Opus"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "no ADC") || strings.Contains(out, "12%\x1b[0m \x1b") {
		t.Errorf("unexpected segment in %q", out)
	}
}

func TestGcloudAdopt(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	dir := t.TempDir()
	write := func(p, s string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), mode); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(dir, "devbox-gcloud-adopt")
	write(p, read("box/devbox-gcloud-adopt"), 0o755)
	// A fake gcloud records the config and token it was given; "1//bad" fails.
	write(filepath.Join(dir, "bin/gcloud"), `#!/bin/bash
read -r tok
[ "$tok" = 1//bad ] && { echo "Refresh token: ERROR: invalid_grant" >&2; exit 1; }
echo "$*" "$tok" > "$CLOUDSDK_CONFIG/activated"
`, 0o755)
	root := filepath.Join(dir, "gcloud")
	write(filepath.Join(root, "cc/legacy_credentials/me@example.com/adc.json"), "{}", 0o600)
	write(filepath.Join(root, "cc/application_default_credentials.json"), `{"refresh_token":"old","quota_project_id":"box-proj"}`, 0o600)
	write(filepath.Join(root, "client/legacy_credentials/me@client.com/adc.json"), "{}", 0o600)
	write(filepath.Join(root, "client/application_default_credentials.json"), `{"refresh_token":"client"}`, 0o600)
	run := func(stdin string) (string, error) {
		c := exec.Command(p, "me@example.com")
		c.Stdin = strings.NewReader(stdin)
		c.Env = []string{"PATH=" + filepath.Join(dir, "bin") + ":" + os.Getenv("PATH"), "DEVBOX_GCLOUD_DIR=" + root}
		out, err := c.CombinedOutput()
		return string(out), err
	}
	adc := func(tok string) string {
		return `{"type":"authorized_user","client_id":"32555940559.apps.googleusercontent.com","refresh_token":"` + tok + `","quota_project_id":"laptop-proj"}`
	}
	readADC := func(cfg string) map[string]string {
		t.Helper()
		var m map[string]string
		b, _ := os.ReadFile(filepath.Join(root, cfg, "application_default_credentials.json"))
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	// Only the config logged in as the account changes; its quota project stays.
	out, err := run(adc("1//new"))
	if err != nil || !strings.Contains(out, "cc: me@example.com logged in, CLI and ADC") {
		t.Fatalf("%v: %s", err, out)
	}
	if m := readADC("cc"); m["refresh_token"] != "1//new" || m["quota_project_id"] != "box-proj" {
		t.Errorf("cc ADC: %v", m)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "cc/activated")); string(b) != "auth activate-refresh-token me@example.com 1//new\n" {
		t.Errorf("cc CLI: %q", b)
	}
	if m := readADC("client"); m["refresh_token"] != "client" {
		t.Errorf("client ADC changed: %v", m)
	}
	// A token the CLI rejects leaves ADC alone too.
	if out, err := run(adc("1//bad")); err != nil || !strings.Contains(out, "rejected") {
		t.Fatalf("%v: %s", err, out)
	}
	if m := readADC("cc"); m["refresh_token"] != "1//new" {
		t.Errorf("cc ADC after a bad token: %v", m)
	}
	// Not a user login: refused.
	if _, err := run(`{"type":"service_account"}`); err == nil {
		t.Error("service account accepted")
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

// make lint names its files; a new box script must not slip past it.
func TestMakeLintCoversBoxScripts(t *testing.T) {
	mk, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range boxFiles {
		switch filepath.Ext(f.src) {
		case ".json", ".conf":
			continue
		}
		if strings.HasSuffix(f.src, "/.ssh/config") || strings.HasSuffix(f.src, "/.gitconfig") {
			continue
		}
		if !strings.Contains(string(mk), strings.TrimPrefix(f.src, "box/")) {
			t.Errorf("%s is not in the Makefile's SCRIPTS or PYTHON", f.src)
		}
	}
}
