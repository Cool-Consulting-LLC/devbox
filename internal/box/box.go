// Package box reconciles one devbox: every resource has a planner that compares
// reality with the config and returns the operations that would make them match.
package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Cool-Consulting-LLC/devbox/internal/config"
	"github.com/Cool-Consulting-LLC/devbox/internal/gcp"
)

type Box struct {
	C   *config.Config
	G   *gcp.Clients
	Out io.Writer
}

func New(c *config.Config, g *gcp.Clients, out io.Writer) *Box {
	return &Box{C: c, G: g, Out: out}
}

// Op is one change. Run fetches fresh state, so ops can depend on earlier ones.
type Op struct {
	Resource   string
	Action     string // create, update, delete, replace, restart, resume, start
	Detail     string
	Disruptive bool // loses guest memory state or the instance itself
	Run        func(ctx context.Context) error
}

func (o Op) String() string {
	sym := map[string]string{"create": "+", "delete": "-", "replace": "-/+", "restart": "~!", "resume": ">", "start": ">", "suspend": "||"}[o.Action]
	if sym == "" {
		sym = "~"
	}
	s := fmt.Sprintf("  %-3s %-34s %s", sym, o.Resource, o.Action)
	if o.Detail != "" {
		s += ": " + o.Detail
	}
	return s
}

// Plan is an ordered list of operations plus things worth knowing that are not ops.
type Plan struct {
	Ops      []Op
	Warnings []string
	// NeedsReprovision is set when provision-tier metadata changes.
	NeedsReprovision bool
}

func (p *Plan) add(op Op)               { p.Ops = append(p.Ops, op) }
func (p *Plan) warn(f string, a ...any) { p.Warnings = append(p.Warnings, fmt.Sprintf(f, a...)) }
func (p *Plan) Empty() bool             { return len(p.Ops) == 0 }
func (p *Plan) Disruptive() (d []Op) {
	for _, o := range p.Ops {
		if o.Disruptive {
			d = append(d, o)
		}
	}
	return d
}

func (p *Plan) Print(w io.Writer) {
	if p.Empty() {
		fmt.Fprintln(w, "  no changes")
	}
	for _, o := range p.Ops {
		fmt.Fprintln(w, o)
	}
	for _, s := range p.Warnings {
		fmt.Fprintln(w, "  ! "+s)
	}
}

// Execute runs ops in order, stopping at the first failure.
func (b *Box) Execute(ctx context.Context, ops []Op) error {
	for _, o := range ops {
		fmt.Fprintf(b.Out, "%s ...\n", strings.TrimSpace(o.String()))
		if err := o.Run(ctx); err != nil {
			return fmt.Errorf("%s %s: %w", o.Action, o.Resource, err)
		}
	}
	return nil
}

// Mode selects how much the reconciler is allowed to do.
type Mode int

const (
	ModeApply Mode = iota // everything, including replace and restart
	ModeUp                // create what is missing and resume; report but skip disruptive changes
)

// Reconcile plans every resource, in dependency order.
func (b *Box) Reconcile(ctx context.Context, mode Mode) (*Plan, error) {
	p := &Plan{}
	steps := []func(context.Context, *Plan) error{
		b.planServiceAccount,
		b.planSuspendRole,
		b.planProjectIAM,
		b.planSecretIAM,
		b.planFirewalls,
		b.planCodeDisk,
		b.planSnapshots,
		func(ctx context.Context, p *Plan) error { return b.planInstance(ctx, p, mode) },
		b.planInstanceIAM,
		b.planAlerting,
		b.checkSecrets,
	}
	for _, s := range steps {
		if err := s(ctx, p); err != nil {
			return nil, err
		}
	}
	if mode == ModeUp {
		var kept []Op
		for _, o := range p.Ops {
			if o.Disruptive {
				p.warn("skipped %s %s (%s): run `devbox apply` to do it", o.Action, o.Resource, o.Detail)
				continue
			}
			kept = append(kept, o)
		}
		p.Ops = kept
	}
	return p, nil
}

func (b *Box) logf(f string, a ...any) { fmt.Fprintf(b.Out, "      "+f+"\n", a...) }

// ErrNotOurs marks a resource found by name that this binary did not create for
// this box. Names are predictable (devbox, devbox-code, ...), so a name match
// alone could be someone else's VM or disk; nothing is changed or deleted
// without the mark the binary leaves: the devbox=<name> label where the
// resource takes labels, its own description where it does not.
var ErrNotOurs = errors.New("not created by devbox for this box")

func (b *Box) notOurs(kind, name, fix string) error {
	return fmt.Errorf("%s %s exists but is %w (%s); refusing to change or delete it. Choose another `name`, or %s",
		kind, name, ErrNotOurs, b.C.Name, fix)
}

func (b *Box) ownsLabels(l map[string]string) bool { return l[config.LabelKey] == b.C.Name }

// managedDesc is the description prefix on resources that take no labels.
func (b *Box) managedDesc() string { return "devbox " + b.C.Name + ": " }

func (b *Box) labelFix(kind, name string) string {
	return fmt.Sprintf("if it really is this box's, label it: gcloud compute %s add-labels %s --labels=%s=%s", kind, name, config.LabelKey, b.C.Name)
}
