package assets

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// Silent Ex mode exits 1 on any error while sourcing the vimrc. HOME is a temp
// directory because the vimrc creates ~/.vim/undo.
func TestVimrcLoads(t *testing.T) {
	if _, err := exec.LookPath("vim"); err != nil {
		t.Skip("vim not installed")
	}
	c := exec.Command("vim", "-Nu", "files/box/skel/.vimrc", "-i", "NONE", "-Es", "+qa!")
	c.Env = append(os.Environ(), "HOME="+t.TempDir())
	if out, err := c.CombinedOutput(); err != nil {
		t.Errorf("vim reported errors loading .vimrc: %v %s", err, out)
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
	// Remote Control off.
	if !strings.Contains(out, "\x1b[31mRC\x1b[0m") {
		t.Errorf("remote control off: %q", out)
	}
	// Remote Control connected.
	if out := run("{}", "CLAUDE_CODE_BRIDGE_SESSION_ID=session_x"); !strings.Contains(out, "\x1b[32mRC\x1b[0m") {
		t.Errorf("remote control: %q", out)
	}
	// A project root holding repos, one with changes.
	proj := filepath.Join(dir, "proj")
	for _, r := range []string{"a", "b"} {
		if out, err := exec.Command("git", "init", "-q", filepath.Join(proj, r)).CombinedOutput(); err != nil {
			t.Fatal(string(out))
		}
	}
	if err := os.WriteFile(filepath.Join(proj, "b", "new"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out = run(`{"workspace":{"current_dir":"` + proj + `","project_dir":"` + proj + `"}}`)
	if !strings.Contains(out, "2 repos\x1b[0m \x1b[33m1*") {
		t.Errorf("project root: %q", out)
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
		case ".json", ".conf", ".service", ".apparmor":
			continue
		}
		if strings.HasSuffix(f.src, "/.ssh/config") || strings.HasSuffix(f.src, "/.gitconfig") ||
			strings.HasSuffix(f.src, "/.vimrc") {
			continue
		}
		if !strings.Contains(string(mk), strings.TrimPrefix(f.src, "box/")) {
			t.Errorf("%s is not in the Makefile's SCRIPTS or PYTHON", f.src)
		}
	}
}

// devbox-project and devbox-sessions against a /srv/code in a temp directory,
// with real git and fakes for tmux and the session script.
func TestDevboxProject(t *testing.T) {
	for _, tool := range []string{"jq", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
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
	readF := func(p string) string {
		b, _ := os.ReadFile(p)
		return string(b)
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	code := filepath.Join(dir, "code")
	claude := filepath.Join(dir, "claude", "projects")
	script := filepath.Join(dir, "devbox-project")
	write(script, read("box/devbox-project"), 0o755)
	write(filepath.Join(dir, "devbox-sessions"), read("box/devbox-sessions"), 0o755)
	write(filepath.Join(code, ".devbox/enrolled", strconv.Itoa(os.Getuid())), "", 0o644)
	write(filepath.Join(dir, "bin/tmux"), `#!/bin/bash
echo "tmux $*" >> "$LOG"
case $1 in has-session) exit 1 ;; display-message) echo other ;; start-server) cat >/dev/null ;; esac
exit 0
`, 0o755)
	write(filepath.Join(dir, "bin/devbox-sessions"), "#!/bin/bash\necho \"devbox-sessions $*\" >> \"$LOG\"\n", 0o755)
	env := []string{"PATH=" + filepath.Join(dir, "bin") + ":" + os.Getenv("PATH"), "HOME=" + dir, "USER=me",
		"DEVBOX_CODE_DIR=" + code, "CLAUDE_CONFIG_DIR=" + filepath.Dir(claude), "LOG=" + filepath.Join(dir, "log"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}
	sh := func(name string, args ...string) (string, error) {
		c := exec.Command(name, args...)
		c.Env = env
		out, err := c.CombinedOutput()
		return string(out), err
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := sh(script, args...)
		if err != nil {
			t.Fatalf("devbox-project %q: %v\n%s", args, err, out)
		}
		return out
	}
	refuse := func(want string, args ...string) {
		t.Helper()
		if out, err := sh(script, args...); err == nil || !strings.Contains(out, want) {
			t.Errorf("devbox-project %q: want a refusal with %q, got %v: %s", args, want, err, out)
		}
	}
	// Two upstream repos, each with a commit.
	for _, r := range []string{"api", "web"} {
		src := filepath.Join(dir, "up", r)
		write(filepath.Join(src, "README"), r, 0o644)
		for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-qm", "init"}} {
			if out, err := sh("git", append([]string{"-C", src}, a...)...); err != nil {
				t.Fatalf("git %v: %v %s", a, err, out)
			}
		}
	}
	envOf := func(p string) map[string]string {
		t.Helper()
		var s struct{ Env map[string]string }
		if err := json.Unmarshal([]byte(readF(p)), &s); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return s.Env
	}
	acme := filepath.Join(code, "acme")
	mangle := func(p string) string {
		return strings.Map(func(r rune) rune {
			if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
				return r
			}
			return '-'
		}, p)
	}

	// add: a new project with its first repo, then a second repo.
	run("add", filepath.Join(dir, "up/api"), "acme", "api", "K=v")
	run("add", filepath.Join(dir, "up/web"), "acme", "web")
	for _, p := range []string{"api/README", "web/README", ".devbox-env"} {
		if !exists(filepath.Join(acme, p)) {
			t.Errorf("missing %s", p)
		}
	}
	for _, f := range []string{".claude", "api/.claude", "web/.claude"} {
		if e := envOf(filepath.Join(acme, f, "settings.local.json")); e["K"] != "v" {
			t.Errorf("%s env = %v", f, e)
		}
	}
	if x := readF(filepath.Join(acme, "web/.git/info/exclude")); !strings.Contains(x, ".claude/settings.local.json\n") {
		t.Errorf("web exclude: %q", x)
	}
	if out, _ := sh("git", "-C", filepath.Join(acme, "api"), "status", "--porcelain"); out != "" {
		t.Errorf("api not clean: %q", out)
	}
	if md := readF(filepath.Join(acme, "CLAUDE.md")); !strings.Contains(md, "- `api/`") {
		t.Errorf("CLAUDE.md: %q", md)
	}
	// set reaches every repo, including a removal.
	run("set", "acme", "-K", "L=w")
	if e := envOf(filepath.Join(acme, "web/.claude/settings.local.json")); e["K"] != "" || e["L"] != "w" {
		t.Errorf("web env after set = %v", e)
	}

	// init: a directory of repos cloned by hand; repos get no settings while there is no env.
	hand := filepath.Join(code, "hand")
	for _, r := range []string{"api", "web"} {
		if out, err := sh("git", "clone", "-q", filepath.Join(dir, "up", r), filepath.Join(hand, r)); err != nil {
			t.Fatal(out)
		}
	}
	run("init", "hand")
	if md := readF(filepath.Join(hand, "CLAUDE.md")); !strings.Contains(md, "- `api/`\n- `web/`\n") {
		t.Errorf("hand CLAUDE.md: %q", md)
	}
	if !exists(filepath.Join(hand, ".claude/settings.local.json")) || exists(filepath.Join(hand, "api/.claude")) {
		t.Error("init settings: want the root's only")
	}

	// nest: an old project that is a repo, with a secret ref and Claude Code state.
	old := filepath.Join(code, "old")
	if out, err := sh("git", "clone", "-q", filepath.Join(dir, "up/api"), old); err != nil {
		t.Fatal(out)
	}
	write(filepath.Join(old, ".devbox-env"), "X=1\nS=@secret:devbox-old-S\n", 0o600)
	write(filepath.Join(old, ".claude/settings.local.json"), `{"permissions":{"allow":[]},"env":{"X":"1"}}`, 0o644)
	write(filepath.Join(claude, mangle(old), "a.jsonl"), `{"cwd":"`+old+`"}`+"\n", 0o644)
	write(filepath.Join(claude, mangle(old), "memory/MEMORY.md"), "m", 0o644)
	write(filepath.Join(claude, mangle(old+"/sub"), "b.jsonl"), `{"type":"x"}`+"\n"+`{"cwd":"`+old+`/sub"}`+"\n", 0o644)
	write(filepath.Join(claude, mangle(old+"-other"), "c.jsonl"), `{"cwd":"`+old+`-other"}`+"\n", 0o644)
	refuse("is itself a repo", "add", filepath.Join(dir, "up/web"), "old", "web")
	refuse("already exists", "nest", "old", "acme", "-")
	refuse("pass --name", "nest", "acme", "acme", "-")
	refuse("only for a project that is itself a repo", "nest", "acme", "x", "api")
	refuse("not a project", "nest", "nope", "x", "-")
	if out := run("secrets", "old"); out != "S devbox-old-S\n" {
		t.Errorf("secrets: %q", out)
	}
	run("nest", "old", "old", "-", "--check")
	if !exists(filepath.Join(old, ".git")) {
		t.Fatal("--check moved something")
	}
	run("nest", "old", "new", "-", "S=@secret:devbox-new-S")
	nw := filepath.Join(code, "new")
	if exists(old) || !exists(filepath.Join(nw, "api/.git")) || exists(filepath.Join(nw, "api/.devbox-env")) {
		t.Fatal("nest did not move the repo into new/api")
	}
	if e := readF(filepath.Join(nw, ".devbox-env")); e != "X=1\nS=@secret:devbox-new-S\n" {
		t.Errorf("new .devbox-env: %q", e)
	}
	if s := readF(filepath.Join(nw, ".claude/settings.local.json")); !strings.Contains(s, "permissions") {
		t.Errorf("root settings lost the repo's other keys: %s", s)
	}
	if !exists(filepath.Join(claude, mangle(nw), "memory/MEMORY.md")) || !exists(filepath.Join(claude, mangle(nw+"/api/sub"), "b.jsonl")) {
		t.Error("Claude Code state did not follow")
	}
	if !exists(filepath.Join(claude, mangle(old+"-other"), "c.jsonl")) {
		t.Error("another project's Claude Code state moved")
	}
	// Rename a project already in the layout.
	run("nest", "new", "newer", "-")
	if !exists(filepath.Join(code, "newer/api/.git")) || !exists(filepath.Join(claude, mangle(filepath.Join(code, "newer")), "a.jsonl")) {
		t.Error("rename did not move the project and its state")
	}
	if log := readF(filepath.Join(dir, "log")); !strings.Contains(log, "tmux kill-session -t =new") {
		t.Errorf("old session not killed: %s", log)
	}

	// devbox-sessions renders .env into the root and each repo, if asked.
	write(filepath.Join(acme, ".devbox-env"), "DEVBOX_DOTENV=1\nA=b\n", 0o600)
	if out, err := sh(filepath.Join(dir, "devbox-sessions")); err != nil {
		t.Fatal(out)
	}
	for _, p := range []string{".env", "api/.env", "web/.env"} {
		if readF(filepath.Join(acme, p)) != "A=b\n" {
			t.Errorf("%s: %q", p, readF(filepath.Join(acme, p)))
		}
	}
}
