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

// getFirewall returns the rule, nil if absent, or the rule with ErrNotOurs if it
// lacks the description this binary writes. Firewall rules take no labels.
func (b *Box) getFirewall(ctx context.Context, name string) (*compute.Firewall, error) {
	fw, err := b.G.Compute.Firewalls.Get(b.C.ProjectID, name).Context(ctx).Do()
	if gcp.IsNotFound(err) {
		return nil, nil
	}
	if err == nil && !strings.HasPrefix(fw.Description, b.managedDesc()) {
		return fw, b.notOurs("firewall rule", name, "delete it if it is a leftover")
	}
	return fw, err
}

func (b *Box) planFirewalls(ctx context.Context, p *Plan) error {
	want := b.desiredFirewalls()
	for _, name := range []string{b.C.FirewallIAPName(), b.C.FirewallGlassName()} {
		have, err := b.getFirewall(ctx, name)
		if err != nil {
			return fmt.Errorf("firewall %s: %w", name, err)
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

// SubnetRange is the range `devbox preflight --fix` gives a subnet it creates.
const SubnetRange = "10.10.0.0/24"

// EnsureNetwork creates the network and subnet devbox.yaml names when they are
// missing: a custom-mode network with no firewall rules, so nothing reaches the
// box but what devbox opens, and a subnet with Private Google Access. It never
// changes or deletes an existing one. Returns what it created.
func (b *Box) EnsureNetwork(ctx context.Context) ([]string, error) {
	var made []string
	_, err := b.G.Compute.Networks.Get(b.C.ProjectID, b.C.Network.Name).Context(ctx).Do()
	if gcp.IsNotFound(err) {
		err = b.G.Await(ctx)(b.G.Compute.Networks.Insert(b.C.ProjectID, &compute.Network{
			Name:                  b.C.Network.Name,
			Description:           b.managedDesc() + "network created by devbox preflight --fix. Never deleted by devbox.",
			AutoCreateSubnetworks: false,
			ForceSendFields:       []string{"AutoCreateSubnetworks"},
		}).Context(ctx).Do())
		if err == nil {
			made = append(made, "network "+b.C.Network.Name)
		}
	}
	if err != nil {
		return made, fmt.Errorf("network %s: %w", b.C.Network.Name, err)
	}
	_, err = b.G.Compute.Subnetworks.Get(b.C.ProjectID, b.C.Region(), b.C.Network.Subnet).Context(ctx).Do()
	if gcp.IsNotFound(err) {
		err = b.G.Await(ctx)(b.G.Compute.Subnetworks.Insert(b.C.ProjectID, b.C.Region(), &compute.Subnetwork{
			Name:                  b.C.Network.Subnet,
			Description:           b.managedDesc() + "subnet created by devbox preflight --fix. Never deleted by devbox.",
			Network:               b.networkURL(),
			IpCidrRange:           SubnetRange,
			PrivateIpGoogleAccess: true,
		}).Context(ctx).Do())
		if err == nil {
			made = append(made, fmt.Sprintf("subnet %s %s", b.C.Network.Subnet, SubnetRange))
		}
	}
	if err != nil {
		return made, fmt.Errorf("subnet %s: %w", b.C.Network.Subnet, err)
	}
	return made, nil
}
