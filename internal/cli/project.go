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

func stringsReader(s string) io.Reader { return strings.NewReader(s) }

func projectCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Add projects to the box and give them secrets",
	}
	cmd.AddCommand(projectAddCmd(g), projectSecretCmd(g))
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

func projectAddCmd(g *globals) *cobra.Command {
	var rf remoteFlags
	var dir, gcloudConfig, gcpProject, region string
	var env []string
	cmd := &cobra.Command{
		Use:   "add <repo>",
		Short: "Clone and register a project on the box",
		Long: `Clones <repo> into /srv/code/<owner>-<repo>/ with the box's machine key, writes
.devbox-env from the flags, writes the same non-secret variables into
.claude/settings.local.json for agent sessions, and creates the project's tmux
session. Running it again updates the variables and leaves the clone alone.

  --gcloud-config NAME  CLOUDSDK_CONFIG, GOOGLE_APPLICATION_CREDENTIALS and
                        KUBECONFIG under /srv/code/.gcloud/NAME
  --gcp-project ID      CLOUDSDK_CORE_PROJECT and GOOGLE_CLOUD_PROJECT
  --region REGION       CLOUDSDK_COMPUTE_REGION
  --env KEY=VALUE       anything else; repeatable. Secrets go through
                        ` + "`devbox project secret`" + `, never here.`,
		Example: `  devbox project add git@github.com:acme/api.git \
    --gcloud-config client-a --gcp-project acme-prod --region europe-west2`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := args[0]
			if dir == "" {
				dir = DirFor(repo)
			}
			if dir == "" {
				return fmt.Errorf("cannot derive a directory from %q; pass --dir", repo)
			}
			var kv []string
			if gcloudConfig != "" {
				base := "/srv/code/.gcloud/" + gcloudConfig
				kv = append(kv, "CLOUDSDK_CONFIG="+base,
					"GOOGLE_APPLICATION_CREDENTIALS="+base+"/application_default_credentials.json",
					"KUBECONFIG="+base+"/kubeconfig")
			}
			if gcpProject != "" {
				kv = append(kv, "CLOUDSDK_CORE_PROJECT="+gcpProject, "GOOGLE_CLOUD_PROJECT="+gcpProject)
			}
			if region != "" {
				kv = append(kv, "CLOUDSDK_COMPUTE_REGION="+region)
			}
			for _, e := range env {
				k, v, ok := strings.Cut(e, "=")
				if !ok || !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(k) {
					return fmt.Errorf("--env %q is not KEY=VALUE", e)
				}
				if strings.HasPrefix(v, "@secret:") {
					return fmt.Errorf("--env %s: use `devbox project secret` for secrets", k)
				}
				kv = append(kv, e)
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
			words := append([]string{"devbox-project", "add", repo, dir}, kv...)
			return t.Run(ctx, remote.Join(words...), nil)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory under /srv/code (default: <owner>-<repo>)")
	cmd.Flags().StringVar(&gcloudConfig, "gcloud-config", "", "gcloud config directory name under /srv/code/.gcloud")
	cmd.Flags().StringVar(&gcpProject, "gcp-project", "", "default GCP project for this project's sessions")
	cmd.Flags().StringVar(&region, "region", "", "default compute region")
	cmd.Flags().StringArrayVar(&env, "env", nil, "extra KEY=VALUE, repeatable")
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
Manager as devbox-<project>-<KEY> labelled devbox=<name>, and sets
KEY=@secret:devbox-<project>-<KEY> in /srv/code/<project>/.devbox-env.

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
			id, err := box.ProjectSecretID(project, key)
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
