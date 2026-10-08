package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/Cool-Consulting-LLC/devbox/internal/assets"
	"github.com/Cool-Consulting-LLC/devbox/internal/box"
	"github.com/Cool-Consulting-LLC/devbox/internal/config"
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
				fmt.Printf("\nTailscale SSH policy users for this box: [%q, \"root\"]\n", res.PosixUser)
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

func upCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Create what is missing, resume if suspended",
		Long: `Creates every missing resource and resumes or starts the instance. One command
whether the box is absent, suspended or stopped.

up never replaces or restarts an existing instance. If devbox.yaml asks for a
change that would, up says so and leaves it for ` + "`devbox apply`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.load(ctx)
			if err != nil {
				return err
			}
			header(b, "up")
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
				fmt.Printf("Watch it with:\n  gcloud compute ssh %s --project %s --zone %s --tunnel-through-iap -- sudo journalctl -fu devbox-provision\n", b.C.Name, b.C.ProjectID, b.C.Zone)
			} else if p.NeedsReprovision {
				fmt.Println("\nMetadata changed: run `devbox reprovision` to apply it on the box.")
			}
			return nil
		},
	}
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
			b, err := g.load(ctx)
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
			b, err := g.load(ctx)
			if err != nil {
				return err
			}
			header(b, "status of")
			return b.Status(ctx, os.Stdout)
		},
	}
}

func applyCmd(g *globals) *cobra.Command {
	var dryRun, yes bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Reconcile config to reality; prints the diff first",
		Long: `Compares what exists with devbox.yaml, prints the operations, then performs them.

  +    create       ~  update in place     >  resume or start
  -/+  replace: the instance is recreated; /srv/code survives, home directories do not
  ~!   restart: the instance is stopped and started; memory state is lost

A suspended instance is always resumed before it is changed. Replace and restart
ask for confirmation unless --yes. --dry-run prints and stops.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.load(ctx)
			if err != nil {
				return err
			}
			header(b, "apply")
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

func reprovisionCmd(g *globals) *cobra.Command {
	var rf remoteFlags
	cmd := &cobra.Command{
		Use:   "reprovision",
		Short: "Push provision.sh from the repo and run it",
		Long: `Updates the instance metadata from devbox.yaml, pushes the provision.sh embedded in
this binary to /srv/code/.devbox/provision.sh over SSH, and runs it. No
replacement, no lost session. Use it after editing provision.sh or any
reprovision-tier key (see docs/02-config.md).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalCtx()
			defer cancel()
			b, err := g.load(ctx)
			if err != nil {
				return err
			}
			in, err := b.SyncProvisionMetadata(ctx)
			if err != nil {
				return err
			}
			if in == nil || in.Status != "RUNNING" {
				return fmt.Errorf("instance %s is not running; `devbox up` first", b.C.Name)
			}
			t, err := rf.target(ctx, b)
			if err != nil {
				return err
			}
			via := t.Host
			if t.IAP {
				via = "IAP"
			}
			fmt.Printf("reprovisioning %s via %s ...\n", b.C.Name, via)
			const script = "set -e; f=/srv/code/.devbox/provision.sh; " +
				"sudo sh -c \"cat > $f.new && chmod 0755 $f.new && mv $f.new $f\"; sudo $f"
			return t.Run(ctx, script, stringsReader(assets.Provision()))
		},
	}
	rf.add(cmd)
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
		Short: "Describe devbox.yaml (--describe writes docs/02-config.md)",
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
