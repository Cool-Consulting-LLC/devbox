package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/Cool-Consulting-LLC/devbox/internal/box"
	"github.com/Cool-Consulting-LLC/devbox/internal/shacrypt"
)

func secretCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Store the secrets devbox.yaml names: Tailscale OAuth, machine key, sudo password",
		Long: `Each subcommand stores one of the secrets devbox.yaml names in Secret Manager,
labelled devbox=<name>. Running one again adds a new version and disables the
older ones. Project secrets are ` + "`devbox project secret`" + `.`,
	}
	cmd.AddCommand(secretTailscaleCmd(g), secretSSHKeyCmd(g), secretSudoCmd(g))
	return cmd
}

func putReport(id string, rotated bool) {
	if rotated {
		fmt.Printf("added a new version of %s; older versions disabled\n", id)
	} else {
		fmt.Printf("created secret %s\n", id)
	}
}

func secretTailscaleCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "tailscale",
		Short: "Store the Tailscale OAuth client secret",
		Long: `Reads the OAuth client secret from stdin (hidden prompt on a terminal) and
stores it as tailscale.oauth_secret. The box gets no standing access to it: it
can read it only for an hour after its code disk is created, or after
` + "`devbox reprovision --rejoin`" + `.`,
		Example: `  devbox secret tailscale
  pbpaste | devbox secret tailscale`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.loadLoggedIn(ctx)
			if err != nil {
				return err
			}
			v, err := readSecret("Tailscale OAuth client secret")
			if err != nil {
				return err
			}
			rotated, err := b.PutBoxSecret(ctx, b.C.Tailscale.OAuthSecret, v)
			if err != nil {
				return err
			}
			putReport(b.C.Tailscale.OAuthSecret, rotated)
			return nil
		},
	}
}

func secretSudoCmd(g *globals) *cobra.Command {
	var reprov bool
	cmd := &cobra.Command{
		Use:   "sudo",
		Short: "Hash a sudo password and store it; reprovision a running box",
		Long: `Asks for the password twice (or reads one line from stdin), stores its
SHA-512 crypt hash as sudo.password_secret, lets the box read it, and
reprovisions a running box so the password works now. Set sudo.password_secret
in devbox.yaml first. The same command rotates the password.

The hash is readable on the box, so use a long password.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.loadLoggedIn(ctx)
			if err != nil {
				return err
			}
			id := b.C.Sudo.PasswordSecret
			if id == "" {
				return errors.New("sudo.password_secret is empty in devbox.yaml; set it to a secret name, e.g. devbox-sudo-password-hash, and run this again")
			}
			pw, err := readNewPassword()
			if err != nil {
				return err
			}
			rotated, err := b.PutBoxSecret(ctx, id, []byte(shacrypt.Hash(pw)))
			if err != nil {
				return err
			}
			putReport(id, rotated)
			return reprovisionIfRunning(ctx, b, reprov)
		},
	}
	cmd.Flags().BoolVar(&reprov, "reprovision", true, "reprovision a running box so the change takes effect")
	return cmd
}

// readNewPassword reads a password twice from the terminal, or once from stdin.
func readNewPassword() ([]byte, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return readSecret("password")
	}
	pw, err := readSecret("sudo password")
	if err != nil {
		return nil, err
	}
	again, err := readSecret("again")
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(pw, again) {
		return nil, errors.New("the passwords differ; nothing stored")
	}
	return pw, nil
}

// reprovisionIfRunning reprovisions the box so a changed secret takes effect,
// and says what to do instead when it cannot.
func reprovisionIfRunning(ctx context.Context, b *box.Box, reprov bool) error {
	in, err := b.GetInstance(ctx)
	if err != nil {
		return err
	}
	switch {
	case in == nil:
		return nil // the first boot provisions with it
	case !reprov || in.Status != "RUNNING":
		fmt.Println("run `devbox reprovision` while the box is running for the change to take effect")
		return nil
	}
	return reprovision(ctx, b, &remoteFlags{}, false)
}

func secretSSHKeyCmd(g *globals) *cobra.Command {
	var replace, github, reprov bool
	cmd := &cobra.Command{
		Use:   "ssh-key",
		Short: "Generate the machine key, store it, and add it to GitHub",
		Long: `Generates an ed25519 key in memory, stores the private key as ssh_key.secret,
and adds the public key to your GitHub account with gh, as both an
authentication and a signing key. No key file is written. If gh lacks the
admin:public_key and admin:ssh_signing_key scopes, it asks for them first.

If the secret exists already, it is kept and its public key is added to GitHub,
so a run that failed half way can be repeated. --replace generates a new key
instead and reprovisions a running box to load it; remove the old key from
GitHub (https://github.com/settings/keys) yourself.

--github=false skips GitHub and prints the public key.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.loadLoggedIn(ctx)
			if err != nil {
				return err
			}
			id := b.C.SSHKey.Secret
			exists, err := b.SecretExists(ctx, id)
			if err != nil {
				return err
			}
			var pub ssh.PublicKey
			if exists && !replace {
				key, err := b.AccessSecret(ctx, id)
				if err != nil {
					return err
				}
				signer, err := ssh.ParsePrivateKey(key)
				if err != nil {
					return fmt.Errorf("secret %s does not hold an SSH private key: %w", id, err)
				}
				pub = signer.PublicKey()
				fmt.Printf("keeping the key in %s (--replace generates a new one)\n", id)
			} else {
				priv, p, err := newMachineKey(b.C.Name)
				if err != nil {
					return err
				}
				pub = p
				rotated, err := b.PutBoxSecret(ctx, id, priv)
				if err != nil {
					return err
				}
				putReport(id, rotated)
			}
			line := bytes.TrimSpace(ssh.MarshalAuthorizedKey(pub))
			line = append(line, " "+b.C.Name...)
			if github {
				if err := githubAddKey(ctx, line, b.C.Name); err != nil {
					return fmt.Errorf("%w\nthe key is stored; run this again to retry, or add this public key to GitHub as an authentication and a signing key:\n%s", err, line)
				}
			} else {
				fmt.Printf("public key:\n%s\n", line)
			}
			if exists && replace {
				return reprovisionIfRunning(ctx, b, reprov)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&replace, "replace", false, "generate a new key even if the secret exists")
	cmd.Flags().BoolVar(&github, "github", true, "add the public key to GitHub with gh")
	cmd.Flags().BoolVar(&reprov, "reprovision", true, "after --replace, reprovision a running box to load the new key")
	return cmd
}

// newMachineKey returns an OpenSSH PEM ed25519 private key and its public key.
func newMachineKey(comment string) ([]byte, ssh.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, nil, err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(block), sp, nil
}

var ghScopes = []string{"admin:public_key", "admin:ssh_signing_key"}

// githubAddKey adds pub to the gh user's account as an authentication and a
// signing key, logging in or asking for the scopes first when gh needs them.
func githubAddKey(ctx context.Context, pub []byte, title string) error {
	if _, err := exec.LookPath("gh"); err != nil {
		return errors.New("gh is not installed")
	}
	if missing, err := missingGhScopes(ctx); err != nil {
		fmt.Println("gh is not logged in; logging in ...")
		if err := ghInteractive(ctx, "auth", "login", "-h", "github.com", "-s", strings.Join(ghScopes, ",")); err != nil {
			return err
		}
	} else if len(missing) > 0 {
		fmt.Printf("gh needs the %s scope(s) to add keys; asking GitHub ...\n", strings.Join(missing, ", "))
		if err := ghInteractive(ctx, "auth", "refresh", "-h", "github.com", "-s", strings.Join(missing, ",")); err != nil {
			return err
		}
	}
	for _, typ := range []string{"authentication", "signing"} {
		c := exec.CommandContext(ctx, "gh", "ssh-key", "add", "-", "--type", typ, "--title", title)
		c.Stdin = bytes.NewReader(append(pub, '\n'))
		out, err := c.CombinedOutput()
		os.Stdout.Write(out)
		if err != nil && !strings.Contains(string(out), "already exists") {
			return fmt.Errorf("gh ssh-key add --type %s: %w", typ, err)
		}
	}
	return nil
}

// missingGhScopes reads the token's scopes from the API's X-Oauth-Scopes header.
// It errs when gh is not logged in, and finds none missing when the token
// reports no scopes (a fine-grained token): gh ssh-key add then says what it lacks.
func missingGhScopes(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, "gh", "api", "-i", "user").Output()
	if err != nil {
		return nil, err
	}
	return MissingScopes(out), nil
}

// MissingScopes reads `gh api -i` output for the ghScopes the token lacks.
func MissingScopes(out []byte) []string {
	head, _, _ := bytes.Cut(out, []byte("\r\n\r\n"))
	for _, l := range strings.Split(string(head), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), ":")
		if !ok || !strings.EqualFold(k, "X-Oauth-Scopes") {
			continue
		}
		have := map[string]bool{}
		for _, s := range strings.Split(v, ",") {
			have[strings.TrimSpace(s)] = true
		}
		var missing []string
		for _, s := range ghScopes {
			if !have[s] {
				missing = append(missing, s)
			}
		}
		return missing
	}
	return nil
}

func ghInteractive(ctx context.Context, args ...string) error {
	c := exec.CommandContext(ctx, "gh", args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("gh %s: %w", strings.Join(args[:2], " "), err)
	}
	return nil
}
