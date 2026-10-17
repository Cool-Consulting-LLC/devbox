package cli

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Cool-Consulting-LLC/devbox/internal/box"
	"github.com/Cool-Consulting-LLC/devbox/internal/remote"
)

func projectCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Add projects and their repos to the box, change their variables, give them secrets",
		Long: `A project is a directory under /srv/code with its own tmux session, variables
and secrets. Its repos are subdirectories, one or many:

  /srv/code/lore/            tmux session "lore"; start claude here
    .devbox-env  CLAUDE.md
    api/  frontend/          repos

Projects made before this layout are a repo themselves; ` + "`devbox project nest`" + `
moves one into it.`,
	}
	cmd.AddCommand(projectAddCmd(g), projectInitCmd(g), projectSetCmd(g), projectSecretCmd(g), projectNestCmd(g))
	return cmd
}

var repoNameRe = regexp.MustCompile(`([^/:]+)/([^/]+?)(\.git)?/?$`)

// DirFor turns git@github.com:acme/api.git into acme-api.
func DirFor(repo string) string {
	m := repoNameRe.FindStringSubmatch(repo)
	if m == nil {
		return ""
	}
	return m[1] + "-" + m[2]
}

// RepoName turns git@github.com:acme/api.git into api.
func RepoName(repo string) string {
	m := repoNameRe.FindStringSubmatch(repo)
	if m == nil {
		return ""
	}
	return m[2]
}

var dirNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func checkDirName(flag, name string) error {
	if !dirNameRe.MatchString(name) {
		return fmt.Errorf("%s %q: use letters, digits, '.', '_' and '-', not starting with '.' or '-'", flag, name)
	}
	return nil
}

var gcloudConfigRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// GcloudEnv points gcloud, client libraries and kubectl at /srv/code/.gcloud/<name>.
func GcloudEnv(name string) ([]string, error) {
	if !gcloudConfigRe.MatchString(name) {
		return nil, fmt.Errorf("--gcloud-config %q: use letters, digits, '.', '_' and '-', not starting with '.' or '-'", name)
	}
	base := "/srv/code/.gcloud/" + name
	return []string{"CLOUDSDK_CONFIG=" + base,
		"GOOGLE_APPLICATION_CREDENTIALS=" + base + "/application_default_credentials.json",
		"KUBECONFIG=" + base + "/kubeconfig"}, nil
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const envFlagsHelp = `  --gcloud-config NAME  CLOUDSDK_CONFIG, GOOGLE_APPLICATION_CREDENTIALS and
                        KUBECONFIG under /srv/code/.gcloud/NAME
  --gcp-project ID      CLOUDSDK_CORE_PROJECT and GOOGLE_CLOUD_PROJECT
  --region REGION       CLOUDSDK_COMPUTE_REGION
  --env KEY=VALUE       anything else; repeatable. Secrets go through
                        ` + "`devbox project secret`" + `, never here.`

// envFlags are the .devbox-env variables `project add` and `project set` share.
type envFlags struct {
	gcloudConfig, gcpProject, region string
	env                              []string
}

func (f *envFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.gcloudConfig, "gcloud-config", "", "gcloud config directory name under /srv/code/.gcloud")
	cmd.Flags().StringVar(&f.gcpProject, "gcp-project", "", "default GCP project for this project's sessions")
	cmd.Flags().StringVar(&f.region, "region", "", "default compute region")
	cmd.Flags().StringArrayVar(&f.env, "env", nil, "extra KEY=VALUE, repeatable")
}

// kv turns the flags into KEY=VALUE words for devbox-project.
func (f *envFlags) kv() ([]string, error) {
	var kv []string
	if f.gcloudConfig != "" {
		gc, err := GcloudEnv(f.gcloudConfig)
		if err != nil {
			return nil, err
		}
		kv = append(kv, gc...)
	}
	if f.gcpProject != "" {
		kv = append(kv, "CLOUDSDK_CORE_PROJECT="+f.gcpProject, "GOOGLE_CLOUD_PROJECT="+f.gcpProject)
	}
	if f.region != "" {
		kv = append(kv, "CLOUDSDK_COMPUTE_REGION="+f.region)
	}
	for _, e := range f.env {
		k, v, ok := strings.Cut(e, "=")
		if !ok || !envKeyRe.MatchString(k) {
			return nil, fmt.Errorf("--env %q is not KEY=VALUE", e)
		}
		if strings.HasPrefix(v, "@secret:") {
			return nil, fmt.Errorf("--env %s: use `devbox project secret` for secrets", k)
		}
		kv = append(kv, e)
	}
	return kv, nil
}

// unsetAliases lets --unset take a flag name for the variables that flag sets.
var unsetAliases = map[string][]string{
	"gcloud-config": {"CLOUDSDK_CONFIG", "GOOGLE_APPLICATION_CREDENTIALS", "KUBECONFIG"},
	"gcp-project":   {"CLOUDSDK_CORE_PROJECT", "GOOGLE_CLOUD_PROJECT"},
	"region":        {"CLOUDSDK_COMPUTE_REGION"},
}

// UnsetWords turns --unset values into -KEY words for devbox-project.
func UnsetWords(unset []string) ([]string, error) {
	var words []string
	for _, u := range unset {
		keys, ok := unsetAliases[u]
		if !ok {
			if !envKeyRe.MatchString(u) {
				return nil, fmt.Errorf("--unset %q: not a variable name or one of gcloud-config, gcp-project, region", u)
			}
			keys = []string{u}
		}
		for _, k := range keys {
			words = append(words, "-"+k)
		}
	}
	return words, nil
}

func projectAddCmd(g *globals) *cobra.Command {
	var rf remoteFlags
	var ef envFlags
	var project, dir string
	cmd := &cobra.Command{
		Use:   "add <repo>",
		Short: "Clone a repo into a project, creating the project if needed",
		Long: `Clones <repo> into /srv/code/<project>/<repo>/ with the box's machine key.
If the project is new, this creates it: .devbox-env from the flags, a CLAUDE.md
for the project root, and the project's tmux session. That CLAUDE.md is a link
to the repo's own if it has one, so git tracks it; else a stub. Flags given for a
project that exists are added to its variables, as ` + "`devbox project set`" + ` does.

The same non-secret variables go into .claude/settings.local.json in the project
root and in each repo: Claude Code reads that file from the git root it starts
in, never from a parent directory.

` + envFlagsHelp,
		Example: `  devbox project add git@github.com:acme/api.git --project acme \
    --gcloud-config client-a --gcp-project acme-prod --region europe-west2
  devbox project add git@github.com:acme/web.git --project acme`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := args[0]
			if project == "" {
				project = DirFor(repo)
			}
			if dir == "" {
				dir = RepoName(repo)
			}
			if project == "" || dir == "" {
				return fmt.Errorf("cannot derive a directory from %q; pass --project and --dir", repo)
			}
			if err := checkDirName("--project", project); err != nil {
				return err
			}
			if err := checkDirName("--dir", dir); err != nil {
				return err
			}
			kv, err := ef.kv()
			if err != nil {
				return err
			}

			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.load(ctx)
			if err != nil {
				return err
			}
			t, err := rf.target(ctx, b)
			if err != nil {
				return err
			}
			words := append([]string{"devbox-project", "add", repo, project, dir}, kv...)
			return t.Run(ctx, remote.Join(words...), nil)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project directory under /srv/code (default: <owner>-<repo>)")
	cmd.Flags().StringVar(&dir, "dir", "", "the clone's directory inside the project (default: <repo>)")
	ef.add(cmd)
	rf.add(cmd)
	return cmd
}

func projectInitCmd(g *globals) *cobra.Command {
	var rf remoteFlags
	var ef envFlags
	cmd := &cobra.Command{
		Use:   "init <project>",
		Short: "Make /srv/code/<project> a project, with repos already in it or none yet",
		Long: `Creates /srv/code/<project>/ if it is missing, then does what ` + "`add`" + ` does
for a new project, without cloning: .devbox-env, CLAUDE.md listing the repos
already there, settings for the root and each repo, and the tmux session. Use it
for a directory of repos you cloned by hand.

` + envFlagsHelp,
		Example: `  devbox project init lore --gcloud-config lore --gcp-project lore-prod`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project := args[0]
			if err := checkDirName("project", project); err != nil {
				return err
			}
			kv, err := ef.kv()
			if err != nil {
				return err
			}
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.load(ctx)
			if err != nil {
				return err
			}
			t, err := rf.target(ctx, b)
			if err != nil {
				return err
			}
			return t.Run(ctx, remote.Join(append([]string{"devbox-project", "init", project}, kv...)...), nil)
		},
	}
	ef.add(cmd)
	rf.add(cmd)
	return cmd
}

// SecretMove is a project secret that a rename stores under a new ID.
type SecretMove struct{ Key, From, To string }

// RenameSecrets reads `devbox-project secrets` output ("KEY ID" lines) and
// returns the secrets of project that are named for it and so must follow a
// rename to newName. A reference to a secret named anything else stays as it is.
func RenameSecrets(boxName, project, newName, refs string) ([]SecretMove, error) {
	var moves []SecretMove
	for _, line := range strings.Split(strings.TrimSpace(refs), "\n") {
		key, id, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		from, err := box.ProjectSecretID(boxName, project, key)
		if err != nil || id != from {
			continue
		}
		to, err := box.ProjectSecretID(boxName, newName, key)
		if err != nil {
			return nil, err
		}
		moves = append(moves, SecretMove{key, from, to})
	}
	return moves, nil
}

func projectNestCmd(g *globals) *cobra.Command {
	var rf remoteFlags
	var newName, repoDir string
	var force bool
	cmd := &cobra.Command{
		Use:   "nest <project>",
		Short: "Move a project that is a repo into the project layout, or rename a project",
		Long: `For a project that is itself a repo, /srv/code/<project>/ with .git in it, moves
the repo to /srv/code/<project>/<repo>/ and leaves .devbox-env, .env and the
settings in the project root. <repo> is the name in its origin URL, or --repo-dir.

--name renames the project too, or only renames one already in the layout:
/srv/code/<project> becomes /srv/code/<name>, its tmux session is renamed, and
its secrets are copied to secrets named for the new project before the old
ones are deleted.

Either way, Claude Code's history and memory for the project and the directories
in it move to the new paths, and the tmux session is restarted, which kills
whatever runs in it. It refuses while Claude Code runs in the project, unless
--force. Claude Code asks once to trust the new directory.`,
		Example: `  devbox project nest acme-api
  devbox project nest acme-api --name acme
  devbox project nest handshake-consulting-Lore --name lore`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project := args[0]
			to := newName
			if to == "" {
				to = project
			}
			sub := repoDir
			if sub == "" {
				sub = "-"
			} else if err := checkDirName("--repo-dir", sub); err != nil {
				return err
			}
			if err := checkDirName("project", project); err != nil {
				return err
			}
			if err := checkDirName("--name", to); err != nil {
				return err
			}
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.load(ctx)
			if err != nil {
				return err
			}
			t, err := rf.target(ctx, b)
			if err != nil {
				return err
			}
			words := []string{"devbox-project", "nest", project, to, sub}
			if force {
				words = append(words, "--force")
			}
			// Everything the box can refuse, before any secret is copied.
			if err := t.Run(ctx, remote.Join(append(words, "--check")...), nil); err != nil {
				return err
			}

			var moves []SecretMove
			if to != project {
				out, err := t.Output(ctx, remote.Join("devbox-project", "secrets", project))
				if err != nil {
					return err
				}
				if moves, err = RenameSecrets(b.C.Name, project, to, string(out)); err != nil {
					return err
				}
			}
			var created []string
			undo := func() {
				for _, id := range created {
					if err := b.DeleteSecret(ctx, id); err != nil {
						fmt.Fprintf(os.Stderr, "could not delete %s: %v\n", id, err)
					}
				}
			}
			for _, m := range moves {
				v, err := b.AccessSecret(ctx, m.From)
				if err != nil {
					undo()
					return fmt.Errorf("reading %s: %w", m.From, err)
				}
				existed, err := b.PutSecret(ctx, m.To, to, v)
				if err != nil {
					undo()
					return fmt.Errorf("storing %s: %w", m.To, err)
				}
				if !existed {
					created = append(created, m.To)
				}
				words = append(words, m.Key+"=@secret:"+m.To)
			}

			if err := t.Run(ctx, remote.Join(words...), nil); err != nil {
				undo()
				return err
			}
			for _, m := range moves {
				if err := b.DeleteSecret(ctx, m.From); err != nil {
					fmt.Fprintf(os.Stderr, "%s is now %s, but deleting the old secret failed: %v\n", m.From, m.To, err)
					continue
				}
				fmt.Printf("secret %s is now %s\n", m.From, m.To)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&newName, "name", "", "rename the project to this")
	cmd.Flags().StringVar(&repoDir, "repo-dir", "", "the repo's directory inside the project (default: its name in the origin URL)")
	cmd.Flags().BoolVar(&force, "force", false, "go ahead even while Claude Code runs in the project")
	rf.add(cmd)
	return cmd
}

func projectSetCmd(g *globals) *cobra.Command {
	var rf remoteFlags
	var ef envFlags
	var unset []string
	var recreate bool
	cmd := &cobra.Command{
		Use:   "set <project>",
		Short: "Change a project's variables",
		Long: `Updates /srv/code/<project>/.devbox-env and the env block in
.claude/settings.local.json. Variables you don't name are left as they are.

` + envFlagsHelp + `
  --unset KEY           remove a variable; repeatable. gcloud-config,
                        gcp-project and region remove what those flags set,
                        so the project falls back to /srv/code/.gcloud/default.
  --recreate            restart the project's tmux session now so it picks up
                        the change. This kills whatever is running in it.

Without --recreate the running session keeps the old values. Claude Code
sessions read settings.local.json at start, so restart those too.`,
		Example: `  devbox project set acme-api --gcloud-config client-b --gcp-project acme-staging
  devbox project set acme-api --unset gcloud-config --recreate
  devbox project set acme-api --env LOG_LEVEL=debug --unset OLD_FLAG`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project := args[0]
			kv, err := ef.kv()
			if err != nil {
				return err
			}
			rm, err := UnsetWords(unset)
			if err != nil {
				return err
			}
			if len(kv)+len(rm) == 0 && !recreate {
				return fmt.Errorf("nothing to change; pass a variable flag, --unset or --recreate")
			}
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.load(ctx)
			if err != nil {
				return err
			}
			t, err := rf.target(ctx, b)
			if err != nil {
				return err
			}
			words := []string{"devbox-project", "set", project}
			if recreate {
				words = append(words, "--recreate")
			}
			words = append(append(words, kv...), rm...)
			return t.Run(ctx, remote.Join(words...), nil)
		},
	}
	ef.add(cmd)
	cmd.Flags().StringArrayVar(&unset, "unset", nil, "remove a variable, or gcloud-config|gcp-project|region; repeatable")
	cmd.Flags().BoolVar(&recreate, "recreate", false, "restart the project's tmux session now")
	rf.add(cmd)
	return cmd
}

func projectSecretCmd(g *globals) *cobra.Command {
	var rf remoteFlags
	var del bool
	cmd := &cobra.Command{
		Use:   "secret <project> <KEY>",
		Short: "Store a project secret, reference it from the env",
		Long: `Reads the value from stdin (hidden prompt on a terminal), stores it in Secret
Manager as <name>-<project>-<KEY> labelled devbox=<name>, grants the box read
access to that one secret, and sets KEY=@secret:<name>-<project>-<KEY> in
/srv/code/<project>/.devbox-env.

Running it again for the same KEY rotates: a new version is added and older
ones are disabled. Sessions read secrets when they are created, so restart the
project's session on the box: devbox-sessions --recreate <project>.

--delete removes the reference and deletes the secret.`,
		Example: `  devbox project secret acme-api OPENAI_API_KEY
  pbpaste | devbox project secret acme-api DATABASE_URL
  devbox project secret acme-api OPENAI_API_KEY --delete`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, key := args[0], args[1]
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.load(ctx)
			if err != nil {
				return err
			}
			id, err := box.ProjectSecretID(b.C.Name, project, key)
			if err != nil {
				return err
			}
			t, err := rf.target(ctx, b)
			if err != nil {
				return err
			}
			if del {
				if err := t.Run(ctx, remote.Join("devbox-project", "secret-unref", project, key), nil); err != nil {
					return err
				}
				if err := b.DeleteSecret(ctx, id); err != nil {
					return err
				}
				fmt.Printf("deleted secret %s\n", id)
				return nil
			}

			value, err := readSecret(key)
			if err != nil {
				return err
			}
			rotated, err := b.PutSecret(ctx, id, project, value)
			if err != nil {
				return err
			}
			if rotated {
				fmt.Printf("added a new version of %s; older versions disabled\n", id)
			} else {
				fmt.Printf("created secret %s\n", id)
			}
			if err := t.Run(ctx, remote.Join("devbox-project", "secret-ref", project, key, id), nil); err != nil {
				fmt.Fprintf(os.Stderr, "the secret is stored, but the box was not updated. On the box, add to /srv/code/%s/.devbox-env:\n  %s=@secret:%s\n", project, key, id)
				return err
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&del, "delete", false, "remove the reference and delete the secret")
	rf.add(cmd)
	return cmd
}

func readSecret(key string) ([]byte, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprintf(os.Stderr, "%s (input hidden): ", key)
		v, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return nil, err
		}
		if len(v) == 0 {
			return nil, fmt.Errorf("empty value")
		}
		return v, nil
	}
	v, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, err
	}
	v = []byte(strings.TrimSuffix(strings.TrimSuffix(string(v), "\n"), "\r"))
	if len(v) == 0 {
		return nil, fmt.Errorf("empty value on stdin")
	}
	return v, nil
}
