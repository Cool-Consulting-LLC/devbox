package box

import (
	"context"
	"fmt"
	"slices"
	"strings"

	compute "google.golang.org/api/compute/v1"

	"github.com/Cool-Consulting-LLC/devbox/internal/gcp"
)

const iapRange = "35.235.240.0/20"

func (b *Box) networkURL() string {
	return fmt.Sprintf("projects/%s/global/networks/%s", b.C.ProjectID, b.C.Network.Name)
}

func (b *Box) subnetURL() string {
	return fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", b.C.ProjectID, b.C.Region(), b.C.Network.Subnet)
}

// Firewall rules target the service account, never a tag: attaching a service
// account to a VM is IAM-gated, a network tag is not.
func (b *Box) desiredFirewalls() map[string]*compute.Firewall {
	fw := map[string]*compute.Firewall{
		b.C.FirewallIAPName(): {
			Name:                  b.C.FirewallIAPName(),
			Description:           "devbox " + b.C.Name + ": SSH from IAP TCP forwarding. Managed by the devbox binary.",
			Network:               b.networkURL(),
			Direction:             "INGRESS",
			SourceRanges:          []string{iapRange},
			Allowed:               []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"22"}}},
			TargetServiceAccounts: []string{b.C.SAEmail()},
		},
	}
	if b.C.Glass.Enabled {
		allowed := []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"22"}}}
		if b.C.Glass.AllowMosh {
			allowed = append(allowed, &compute.FirewallAllowed{IPProtocol: "udp", Ports: []string{"60000-61000"}})
		}
		fw[b.C.FirewallGlassName()] = &compute.Firewall{
			Name:                  b.C.FirewallGlassName(),
			Description:           "devbox " + b.C.Name + ": break-glass sshd from one address. Managed by the devbox binary.",
			Network:               b.networkURL(),
			Direction:             "INGRESS",
			SourceRanges:          []string{b.C.Glass.SourceCIDR},
			Allowed:               allowed,
			TargetServiceAccounts: []string{b.C.SAEmail()},
		}
	}
	return fw
}

func fwDiff(have, want *compute.Firewall) string {
	var d []string
	if gcp.Last(have.Network) != gcp.Last(want.Network) {
		d = append(d, "network")
	}
	if !slices.Equal(have.SourceRanges, want.SourceRanges) {
		d = append(d, fmt.Sprintf("source %v -> %v", have.SourceRanges, want.SourceRanges))
	}
	if !slices.Equal(have.TargetServiceAccounts, want.TargetServiceAccounts) || len(have.TargetTags) > 0 {
		d = append(d, "target")
	}
	if allowedString(have.Allowed) != allowedString(want.Allowed) {
		d = append(d, fmt.Sprintf("ports %s -> %s", allowedString(have.Allowed), allowedString(want.Allowed)))
	}
	if have.Disabled {
		d = append(d, "enable")
	}
	return strings.Join(d, ", ")
}

func allowedString(a []*compute.FirewallAllowed) string {
	var s []string
	for _, x := range a {
		s = append(s, x.IPProtocol+":"+strings.Join(x.Ports, ","))
	}
	slices.Sort(s)
	return strings.Join(s, " ")
}

func (b *Box) planFirewalls(ctx context.Context, p *Plan) error {
	want := b.desiredFirewalls()
	for _, name := range []string{b.C.FirewallIAPName(), b.C.FirewallGlassName()} {
		have, err := b.G.Compute.Firewalls.Get(b.C.ProjectID, name).Context(ctx).Do()
		if err != nil && !gcp.IsNotFound(err) {
			return fmt.Errorf("firewall %s: %w", name, err)
		}
		if err != nil {
			have = nil
		}
		w := want[name]
		res := "firewall " + name
		switch {
		case have == nil && w == nil:
		case have == nil:
			p.add(Op{Resource: res, Action: "create", Detail: fmt.Sprintf("%s from %v", allowedString(w.Allowed), w.SourceRanges), Run: func(ctx context.Context) error {
				return b.G.Await(ctx)(b.G.Compute.Firewalls.Insert(b.C.ProjectID, w).Context(ctx).Do())
			}})
		case w == nil:
			p.add(Op{Resource: res, Action: "delete", Run: func(ctx context.Context) error {
				return b.G.Await(ctx)(b.G.Compute.Firewalls.Delete(b.C.ProjectID, name).Context(ctx).Do())
			}})
		default:
			d := fwDiff(have, w)
			if d == "" {
				continue
			}
			if gcp.Last(have.Network) != gcp.Last(w.Network) {
				p.add(Op{Resource: res, Action: "replace", Detail: d, Run: func(ctx context.Context) error {
					if err := b.G.Await(ctx)(b.G.Compute.Firewalls.Delete(b.C.ProjectID, name).Context(ctx).Do()); err != nil {
						return err
					}
					return b.G.Await(ctx)(b.G.Compute.Firewalls.Insert(b.C.ProjectID, w).Context(ctx).Do())
				}})
				continue
			}
			p.add(Op{Resource: res, Action: "update", Detail: d, Run: func(ctx context.Context) error {
				return b.G.Await(ctx)(b.G.Compute.Firewalls.Update(b.C.ProjectID, name, w).Context(ctx).Do())
			}})
		}
	}
	return nil
}
