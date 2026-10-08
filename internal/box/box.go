// Package box reconciles one devbox: every resource has a planner that compares
// reality with the config and returns the operations that would make them match.
package box

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/Cool-Consulting-LLC/devbox/internal/config"
	"github.com/Cool-Consulting-LLC/devbox/internal/gcp"
)

type Box struct {
	C   *config.Config
	G   *gcp.Clients
	Out io.Writer

	numOnce sync.Once
	num     string
	numErr  error
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

// ProjectNumber is needed for Secret Manager IAM conditions, which match on it.
func (b *Box) ProjectNumber(ctx context.Context) (string, error) {
	b.numOnce.Do(func() {
		pr, err := b.G.CRM.Projects.Get(b.C.ProjectID).Context(ctx).Do()
		if err != nil {
			b.numErr = err
			return
		}
		b.num = fmt.Sprint(pr.ProjectNumber)
	})
	return b.num, b.numErr
}

func (b *Box) logf(f string, a ...any) { fmt.Fprintf(b.Out, "      "+f+"\n", a...) }
