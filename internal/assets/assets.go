// Package assets embeds and renders everything that runs on the box.
package assets

import (
	"bytes"
	"embed"
	"fmt"
	"sort"
	"strings"
	"text/template"

	"github.com/Cool-Consulting-LLC/devbox/internal/config"
)

//go:embed all:files
var files embed.FS

func read(name string) string {
	b, err := files.ReadFile("files/" + name)
	if err != nil {
		panic(err) // embedded; cannot fail at runtime
	}
	return string(b)
}

// boxFile is a file provision.sh writes on every run.
type boxFile struct{ src, dst, mode string }

// BoxFiles are reprovision tier: edit them, run `devbox reprovision`.
// Claude Code policy is here rather than in cloud-init so that changing it
// never replaces the instance. write_files runs before claude-code is
// installed, so no session ever starts without it.
var boxFiles = []boxFile{
	{"box/managed-settings.json", "/etc/claude-code/managed-settings.json", "0644"},
	{"box/guard.sh", "/etc/claude-code/hooks/guard.sh", "0755"},
	{"box/claude-activity.sh", "/etc/claude-code/hooks/claude-activity.sh", "0755"},
	{"box/devbox-secret", "/usr/local/bin/devbox-secret", "0755"},
	{"box/devbox-sessions", "/usr/local/bin/devbox-sessions", "0755"},
	{"box/devbox-project", "/usr/local/bin/devbox-project", "0755"},
	{"box/devbox-gcloud-login", "/usr/local/bin/devbox-gcloud-login", "0755"},
	{"box/devbox-tmux-colour", "/usr/local/bin/devbox-tmux-colour", "0755"},
	{"box/devbox-claude-statusline", "/usr/local/bin/devbox-claude-statusline", "0755"},
	{"box/devbox-agent-relay", "/usr/local/sbin/devbox-agent-relay", "0755"},
	{"box/devbox-agent-load-key", "/usr/local/sbin/devbox-agent-load-key", "0755"},
	{"box/devbox-ssh-agent.conf", "/etc/systemd/system/devbox-ssh-agent.service.d/devbox-load-key.conf", "0644"},
	{"box/needrestart.conf", "/etc/needrestart/conf.d/devbox.conf", "0644"},
	{"box/devbox-enroll", "/usr/local/sbin/devbox-enroll", "0755"},
	{"box/mosh-server", "/usr/local/bin/mosh-server", "0755"},
	{"box/idle-check.sh", "/usr/local/sbin/devbox-idle-check", "0755"},
	{"box/profile.sh", "/etc/profile.d/devbox.sh", "0644"},
	{"box/tmux.conf", "/etc/tmux.conf", "0644"},
	{"box/skel/.bashrc", "/etc/skel/.bashrc", "0644"},
	{"box/skel/.gitconfig", "/etc/skel/.gitconfig", "0644"},
	{"box/skel/.ssh/config", "/etc/skel/.ssh/config", "0600"},
}

// Provision renders provision.sh with the box files inlined as quoted heredocs,
// so one metadata value or one SSH push carries everything.
func Provision() string {
	var b strings.Builder
	b.WriteString("write_files() {\n")
	for i, f := range boxFiles {
		body := read(f.src)
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		delim := fmt.Sprintf("DEVBOX_EOF_%d", i)
		if strings.Contains(body, delim) {
			panic("heredoc delimiter collides with " + f.src)
		}
		tmp := f.dst + ".devbox-new"
		fmt.Fprintf(&b, "  install -d -m 0755 %s\n", dir(f.dst))
		fmt.Fprintf(&b, "  cat > %s <<'%s'\n%s%s\n", tmp, delim, body, delim)
		fmt.Fprintf(&b, "  chmod %s %s && mv %s %s\n", f.mode, tmp, tmp, f.dst)
	}
	b.WriteString("  chmod 0700 /etc/skel/.ssh\n}\n")
	return strings.Replace(read("provision.sh"), "# @@WRITE_FILES@@\n", b.String(), 1)
}

func dir(p string) string { return p[:strings.LastIndex(p, "/")] }

// CloudInit renders the user-data. Replace tier: a different result recreates the instance.
func CloudInit() string {
	t := template.Must(template.New("ci").Funcs(template.FuncMap{
		"indent": func(n int, s string) string {
			pad := strings.Repeat(" ", n)
			lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
			for i, l := range lines {
				if l != "" {
					lines[i] = pad + l
				}
			}
			return strings.Join(lines, "\n")
		},
	}).Parse(read("cloud-init.yaml")))
	var b bytes.Buffer
	if err := t.Execute(&b, map[string]string{
		"Bootstrap": read("bootstrap.sh"),
	}); err != nil {
		panic(err)
	}
	return b.String()
}

// ConfigEnv renders the non-secret config provision.sh reads from instance metadata.
// Shell-sourceable; values single-quoted.
func ConfigEnv(c *config.Config) string {
	vars := map[string]string{
		"DEVBOX_NAME":           c.Name,
		"DEVBOX_GIT_NAME":       c.Git.Name,
		"DEVBOX_GIT_EMAIL":      c.Git.Email,
		"DEVBOX_TS_TAG":         c.Tailscale.Tag,
		"DEVBOX_TS_SECRET":      c.Tailscale.OAuthSecret,
		"DEVBOX_SSH_KEY_SECRET": c.SSHKey.Secret,
		"DEVBOX_IDLE_ENABLED":   fmt.Sprint(c.IdleEnabled()),
		"DEVBOX_IDLE_MINUTES":   fmt.Sprint(c.Idle.AfterMinutes),
		"DEVBOX_EXTRA_PACKAGES": strings.Join(c.ExtraPackages, " "),
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s='%s'\n", k, strings.ReplaceAll(vars[k], "'", `'\''`))
	}
	return b.String()
}

// GuardHook is exposed for tests.
func GuardHook() string { return read("box/guard.sh") }
