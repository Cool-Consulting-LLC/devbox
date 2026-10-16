package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Cool-Consulting-LLC/devbox/internal/box"
	"github.com/Cool-Consulting-LLC/devbox/internal/config"
	"github.com/Cool-Consulting-LLC/devbox/internal/gcp"
	"github.com/Cool-Consulting-LLC/devbox/internal/remote"
)

func signalCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

func preflightCmd(g *globals) *cobra.Command {
	var fix bool
	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Read-only survey of the project; --fix enables missing APIs",
		Long: `Checks everything ` + "`up`" + ` depends on and prints one line per check, with the fix
for each failure. Exits non-zero if anything is FAIL or MISSING.

--fix enables missing APIs and changes nothing else. APIs are never disabled.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.load(ctx)
			if err != nil {
				return err
			}
			header(b, "preflight")
			res, err := b.Preflight(ctx, fix, os.Stdout)
			if err != nil {
				return err
			}
			if res.PosixUser != "" {
				fmt.Printf("\nTailscale SSH rule for this box (README.md, How to set it up, step 3):\n")
				fmt.Printf("  { \"action\": \"accept\", \"src\": [%q], \"dst\": [\"tag:devbox\"], \"users\": [%q] }\n", res.Email, res.PosixUser)
			}
			if !res.OK {
				return errors.New("preflight failed")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "enable missing APIs")
	return cmd
}

// exposureGuard refuses to go on when the box would be open to the internet,
// unless allow, in which case it says so and goes on.
func exposureGuard(ctx context.Context, b *box.Box, allow bool) error {
	err := b.CheckExposure(ctx)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, box.ErrExposed):
		return err
	case !allow:
		return fmt.Errorf("%w\n--allow-exposure goes ahead anyway", err)
	}
	fmt.Printf("  ! %v\n  ! going ahead anyway: --allow-exposure\n\n", err)
	return nil
}

func upCmd(g *globals) *cobra.Command {
	var allowExposure, shareLogin bool
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create what is missing, resume if suspended",
		Long: `Creates every missing resource and resumes or starts the instance. One command
whether the box is absent, suspended or stopped.

up never replaces or restarts an existing instance. If devbox.yaml asks for a
change that would, up says so and leaves it for ` + "`devbox apply`." + `

up refuses to run while the box has an external IP and a firewall rule devbox
did not create admits the internet (see ` + "`devbox preflight`" + `), unless
--allow-exposure.

If your Google login is missing or expired, up first runs
` + "`gcloud auth login --update-adc`" + `, as ` + "`make auth`" + ` does, when at a terminal.
From the laptop, up then passes your Google login to the box: every gcloud
config there logged in as the same account gets it, CLI login and ADC, so one
login covers both.
--share-login=false skips it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.loadLoggedIn(ctx)
			if err != nil {
				return err
			}
			header(b, "up")
			fmt.Printf("at %s\n", time.Now().Format("2006-01-02 15:04:05 MST"))
			if err := exposureGuard(ctx, b, allowExposure); err != nil {
				return err
			}
			created := false
			p, err := b.Reconcile(ctx, box.ModeUp)
			if err != nil {
				return err
			}
			for _, o := range p.Ops {
				created = created || (o.Action == "create" && o.Resource == "instance "+b.C.Name)
			}
			p.Print(os.Stdout)
			if err := b.Execute(ctx, p.Ops); err != nil {
				return err
			}
			if created {
				fmt.Printf("\nThe box is booting. Provisioning takes a few minutes. First login, which creates your account:\n")
				fmt.Printf("  gcloud compute ssh %s --project %s --zone %s --tunnel-through-iap\n", b.C.Name, b.C.ProjectID, b.C.Zone)
				fmt.Printf("Watch it with:\n  gcloud compute ssh %s --project %s --zone %s --tunnel-through-iap -- tail -f /var/log/devbox-provision.log\n", b.C.Name, b.C.ProjectID, b.C.Zone)
			} else if p.NeedsReprovision {
				fmt.Println("\nMetadata changed: run `devbox reprovision` to apply it on the box.")
			}
			// A new box has no account for you yet, and on the box the
			// login is the box's own already.
			if shareLogin && !created && !onBox(b.C.Name) {
				if err := adoptLogin(ctx, b); err != nil {
					fmt.Fprintf(os.Stderr, "could not pass your login to the box: %v\n", err)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&allowExposure, "allow-exposure", false, "go ahead even if firewall rules open the box to the internet")
	cmd.Flags().BoolVar(&shareLogin, "share-login", true, "pass your Google login to the box's gcloud configs for the same account")
	return cmd
}

// adoptLogin gives the box the laptop's Google login, so the reauthentication
// that let up run covers the box's gcloud configs for the same account too. It
// does nothing unless the credentials are a user login.
func adoptLogin(ctx context.Context, b *box.Box) error {
	js := b.G.UserCredentialJSON()
	if js == nil {
		return nil
	}
	email, err := b.G.CallerEmail(ctx)
	if err != nil {
		return err
	}
	t, err := (&remoteFlags{}).target(ctx, b)
	if err != nil {
		return err
	}
	fmt.Printf("\npassing your login (%s) to the box ...\n", email)
	if err := t.WaitReachable(ctx, 90*time.Second); err != nil {
		return err
	}
	err = t.Run(ctx, remote.Join("devbox-gcloud-adopt", email), bytes.NewReader(js))
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 127 {
		return errors.New("the box has no devbox-gcloud-adopt yet; run `devbox reprovision`, then `devbox up` again")
	}
	return err
}

func downCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Suspend, preserving memory state",
		Long: `Suspends the instance. Guest memory is saved, so tmux and running Claude Code
sessions survive. vCPU billing stops; disks and suspended RAM still bill.
After 60 days suspended, Compute Engine stops the instance and memory is lost.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.loadLoggedIn(ctx)
			if err != nil {
				return err
			}
			in, err := b.GetInstance(ctx)
			if err != nil {
				return err
			}
			switch {
			case in == nil:
				return fmt.Errorf("instance %s does not exist", b.C.Name)
			case in.Status != "RUNNING":
				fmt.Printf("%s is %s; nothing to do\n", b.C.Name, in.Status)
				return nil
			}
			fmt.Printf("suspending %s ...\n", b.C.Name)
			if err := b.Suspend(ctx); err != nil {
				return err
			}
			fmt.Println("suspended. `devbox up` resumes it.")
			return nil
		},
	}
}

func statusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "What exists, its state, what it is costing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.loadLoggedIn(ctx)
			if err != nil {
				return err
			}
			header(b, "status of")
			return b.Status(ctx, os.Stdout)
		},
	}
}

func applyCmd(g *globals) *cobra.Command {
	var dryRun, yes, allowExposure bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Reconcile config to reality; prints the diff first",
		Long: `Compares what exists with devbox.yaml, prints the operations, then performs them.

  +    create       ~  update in place     >  resume or start
  -/+  replace: the instance is recreated; /srv/code survives, home directories do not
  ~!   restart: the instance is stopped and started; memory state is lost

A suspended instance is always resumed before it is changed. Replace and restart
ask for confirmation unless --yes. --dry-run prints and stops.

Like up, it refuses to run while the box has an external IP and a firewall rule
devbox did not create admits the internet, unless --allow-exposure. --dry-run
reports such rules and goes on.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.load(ctx)
			if err != nil {
				return err
			}
			header(b, "apply")
			if err := exposureGuard(ctx, b, allowExposure || dryRun); err != nil {
				return err
			}
			p, err := b.Reconcile(ctx, box.ModeApply)
			if err != nil {
				return err
			}
			p.Print(os.Stdout)
			if dryRun || p.Empty() {
				return nil
			}
			if d := p.Disruptive(); len(d) > 0 && !yes {
				if err := confirm(fmt.Sprintf("\n%d operation(s) will end running sessions on %s.", len(d), b.C.Name), "yes"); err != nil {
					return err
				}
			}
			if err := b.Execute(ctx, p.Ops); err != nil {
				return err
			}
			if p.NeedsReprovision {
				fmt.Println("\nReprovision-tier settings changed: run `devbox reprovision`.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the operations and stop")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask before replace or restart")
	cmd.Flags().BoolVar(&allowExposure, "allow-exposure", false, "go ahead even if firewall rules open the box to the internet")
	return cmd
}

type remoteFlags struct {
	host, user string
	iap        bool
}

func (r *remoteFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&r.host, "host", "", "tailnet hostname (default: the box name)")
	cmd.Flags().StringVar(&r.user, "user", "", "login name (default: your OS Login POSIX name)")
	cmd.Flags().BoolVar(&r.iap, "iap", false, "connect through IAP with gcloud instead of the tailnet")
}

func (r *remoteFlags) target(ctx context.Context, b *box.Box) (remote.Target, error) {
	t := remote.Target{Host: r.host, User: r.user, IAP: r.iap, Project: b.C.ProjectID, Zone: b.C.Zone, Instance: b.C.Name}
	if t.Host == "" {
		t.Host = b.C.Name
		if !t.IAP && onBox(b.C.Name) {
			// SSH from the box to itself fails either way: Tailscale SSH
			// refuses it and the VPC sshd has no key for us.
			me := currentUser()
			if t.User != "" && t.User != me {
				return t, fmt.Errorf("on the box as %s, --user %s cannot SSH to the box itself; run it as %s, or from the laptop", me, t.User, t.User)
			}
			t.Local = true
			return t, nil
		}
	}
	if t.User == "" && !t.IAP {
		u, err := b.PosixUser(ctx)
		if err != nil {
			return t, fmt.Errorf("cannot find your OS Login name (pass --user): %w", err)
		}
		t.User = u
	}
	return t, nil
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

// onBox reports whether this process runs on the box called name.
func onBox(name string) bool {
	h, err := os.Hostname()
	if err != nil {
		return false
	}
	short, _, _ := strings.Cut(h, ".")
	return short == name
}

// reprovisionScript restarts the provision unit, which fetches provision.sh from
// metadata, and shows this run's part of the log. Restarting it is the one sudo
// rule the box has besides enrolment; it runs only what metadata holds.
//
// sudo runs with no terminal: setsid drops the controlling one, and no standard
// stream is a tty. Ubuntu sets use_pty, so sudo given a terminal puts it in raw
// mode for the command's lifetime, and the tail's lines, which go straight to
// that terminal, then lose their carriage returns and staircase across it.
const reprovisionScript = `log=/var/log/devbox-provision.log
start=$(( $(stat -c %s "$log" 2>/dev/null || echo 0) + 1 ))
tail -c "+$start" -F "$log" 2>/dev/null & t=$!
out=$(setsid -w sudo -n systemctl restart devbox-provision.service </dev/null 2>&1); rc=$?
sleep 1; kill "$t" 2>/dev/null
[ -z "$out" ] || printf '%s\n' "$out" >&2
exit "$rc"`

func reprovisionCmd(g *globals) *cobra.Command {
	var rf remoteFlags
	var rejoin bool
	cmd := &cobra.Command{
		Use:   "reprovision",
		Short: "Push provision.sh from the repo and run it",
		Long: `Writes the provision.sh embedded in this binary, and the config from
devbox.yaml, to the instance metadata, then has the box re-run it as root. No
replacement, no lost session. Use it after editing provision.sh or any
reprovision-tier key (see docs/config.md).

Runs anywhere: on the laptop it reaches the box over Tailscale SSH (or --iap);
on the box it runs locally. Either way it logs in as you. Root on the box only
ever runs what metadata holds, so the Google credentials here must be able to
set the instance's metadata. On the box, that means your own
` + "`gcloud auth application-default login`" + `: the box's service account cannot.

The box can read the Tailscale OAuth secret only for an hour after its code disk
is created. If it has not joined the tailnet by then, --rejoin opens another
hour before the re-run; use it with --iap, since the tailnet is what is missing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.loadLoggedIn(ctx)
			if err != nil {
				return err
			}
			in, err := b.SyncProvisionMetadata(ctx)
			if gcp.Code(err) == 403 {
				return fmt.Errorf("%w\nthese Google credentials cannot update the instance's metadata. On the box without your own `gcloud auth application-default login`, they are the box's service account, which by design cannot", err)
			}
			if err != nil {
				return err
			}
			if in == nil || in.Status != "RUNNING" {
				return fmt.Errorf("instance %s is not running; `devbox up` first", b.C.Name)
			}
			if rejoin {
				if err := b.GrantTailnetJoin(ctx); err != nil {
					return fmt.Errorf("letting the box read %s: %w", b.C.Tailscale.OAuthSecret, err)
				}
				fmt.Printf("%s can read %s for the next hour\n", b.C.Name, b.C.Tailscale.OAuthSecret)
			}
			t, err := rf.target(ctx, b)
			if err != nil {
				return err
			}
			via := t.Host
			if t.Local {
				via = "this machine"
			} else if t.IAP {
				via = "IAP"
			}
			fmt.Printf("reprovisioning %s via %s ...\n", b.C.Name, via)
			return t.Run(ctx, reprovisionScript, nil)
		},
	}
	rf.add(cmd)
	cmd.Flags().BoolVar(&rejoin, "rejoin", false, "let the box read the Tailscale OAuth secret for an hour, to join the tailnet")
	return cmd
}

func destroyCmd(g *globals) *cobra.Command {
	var all, yes bool
	var confirmDisk string
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Tear down; --all includes the code disk",
		Long: `Deletes everything this box created, found by label and name prefix, except:
  - the code disk and its snapshots
  - secrets labelled devbox=<name>
  - the shared custom role ` + config.SuspendRoleID + `
--all deletes those too, and asks you to type the code disk's name.

Never touches the project, the VPC, the subnet or API enablement.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.load(ctx)
			if err != nil {
				return err
			}
			header(b, "destroy")
			p, err := b.PlanDestroy(ctx, all)
			if err != nil {
				return err
			}
			p.Print(os.Stdout)
			if p.Empty() {
				return nil
			}
			if all {
				if confirmDisk != b.C.CodeDiskName() {
					if err := confirm("\nThis deletes /srv/code and every snapshot of it.", b.C.CodeDiskName()); err != nil {
						return err
					}
				}
			} else if !yes {
				if err := confirm("", "yes"); err != nil {
					return err
				}
			}
			if err := b.Execute(ctx, p.Ops); err != nil {
				return err
			}
			if !all {
				fmt.Printf("\nKept: disk %s, its snapshots, and devbox-* secrets. `devbox destroy --all` removes them.\n", b.C.CodeDiskName())
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "also delete the code disk, its snapshots, this box's secrets and the custom role")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask (does not cover --all)")
	cmd.Flags().StringVar(&confirmDisk, "confirm-disk", "", "the code disk name, to confirm --all without a terminal")
	return cmd
}

func configCmd() *cobra.Command {
	var describe bool
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Describe devbox.yaml (--describe writes docs/config.md)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !describe {
				return cmd.Help()
			}
			return config.WriteMarkdown(os.Stdout)
		},
	}
	cmd.Flags().BoolVar(&describe, "describe", false, "print every key as Markdown")
	return cmd
}
