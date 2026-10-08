package box

import (
	"context"
	"fmt"

	crm "google.golang.org/api/cloudresourcemanager/v1"
	compute "google.golang.org/api/compute/v1"

	"github.com/Cool-Consulting-LLC/devbox/internal/config"
	"github.com/Cool-Consulting-LLC/devbox/internal/gcp"
)

// PlanDestroy enumerates by label and name prefix; there is no state file to read.
// Never touches the project, the VPC, the subnet or API enablement.
func (b *Box) PlanDestroy(ctx context.Context, all bool) (*Plan, error) {
	p := &Plan{}

	pols, err := b.listPolicies(ctx)
	if err != nil {
		return nil, err
	}
	for _, pol := range pols {
		p.add(b.deletePolicyOp(pol))
	}
	chans, err := b.listChannels(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range chans {
		p.add(b.deleteChannelOp(c))
	}

	in, err := b.GetInstance(ctx)
	if err != nil {
		return nil, err
	}
	if in != nil {
		p.add(Op{Resource: "instance " + b.C.Name, Action: "delete", Detail: "boot disk with it; the code disk is detached, not deleted", Run: func(ctx context.Context) error {
			return b.G.Await(ctx)(b.G.Compute.Instances.Delete(b.C.ProjectID, b.C.Zone, b.C.Name).Context(ctx).Do())
		}})
	}

	disk, err := b.getCodeDisk(ctx)
	if err != nil {
		return nil, err
	}
	rp, err := b.getSnapshotPolicy(ctx)
	if err != nil {
		return nil, err
	}
	if all && disk != nil {
		p.add(Op{Resource: "disk " + disk.Name, Action: "delete", Detail: fmt.Sprintf("%d GB, everything in /srv/code", disk.SizeGb), Run: func(ctx context.Context) error {
			return b.G.Await(ctx)(b.G.Compute.Disks.Delete(b.C.ProjectID, b.C.Zone, disk.Name).Context(ctx).Do())
		}})
	}
	if rp != nil {
		if !all && disk != nil && len(disk.ResourcePolicies) > 0 {
			p.add(Op{Resource: "snapshot schedule " + rp.Name, Action: "update", Detail: "detach from " + disk.Name, Run: func(ctx context.Context) error {
				return b.G.Await(ctx)(b.G.Compute.Disks.RemoveResourcePolicies(b.C.ProjectID, b.C.Zone, disk.Name,
					&compute.DisksRemoveResourcePoliciesRequest{ResourcePolicies: []string{rp.SelfLink}}).Context(ctx).Do())
			}})
		}
		p.add(Op{Resource: "snapshot schedule " + rp.Name, Action: "delete", Run: func(ctx context.Context) error {
			return b.G.Await(ctx)(b.G.Compute.ResourcePolicies.Delete(b.C.ProjectID, b.C.Region(), rp.Name).Context(ctx).Do())
		}})
	}
	if all {
		snaps, err := b.ListSnapshots(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range snaps {
			p.add(Op{Resource: "snapshot " + s.Name, Action: "delete", Run: func(ctx context.Context) error {
				return b.G.Await(ctx)(b.G.Compute.Snapshots.Delete(b.C.ProjectID, s.Name).Context(ctx).Do())
			}})
		}
	}

	for _, name := range []string{b.C.FirewallIAPName(), b.C.FirewallGlassName()} {
		_, err := b.G.Compute.Firewalls.Get(b.C.ProjectID, name).Context(ctx).Do()
		if gcp.IsNotFound(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		p.add(Op{Resource: "firewall " + name, Action: "delete", Run: func(ctx context.Context) error {
			return b.G.Await(ctx)(b.G.Compute.Firewalls.Delete(b.C.ProjectID, name).Context(ctx).Do())
		}})
	}

	pol, err := b.getProjectPolicy(ctx)
	if err != nil {
		return nil, err
	}
	roles := append([]string{secretRole}, agentRoles...)
	if removeMember(pol, b.C.SAMember(), roles...) {
		p.add(Op{Resource: "project IAM", Action: "update", Detail: "revoke " + b.C.SAEmail(), Run: func(ctx context.Context) error {
			return b.editProjectPolicy(ctx, func(pol *crm.Policy) { removeMember(pol, b.C.SAMember(), roles...) })
		}})
	}

	sa, err := b.getSA(ctx)
	if err != nil {
		return nil, err
	}
	if sa != nil {
		p.add(Op{Resource: "service account " + b.C.Name, Action: "delete", Run: func(ctx context.Context) error {
			_, err := b.G.IAM.Projects.ServiceAccounts.Delete(b.saName()).Context(ctx).Do()
			if gcp.IsNotFound(err) {
				return nil
			}
			return err
		}})
	}

	if !all {
		return p, nil
	}
	secrets, err := b.ListLabelledSecrets(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range secrets {
		id := gcp.Last(s.Name)
		p.add(Op{Resource: "secret " + id, Action: "delete", Run: func(ctx context.Context) error { return b.DeleteSecret(ctx, id) }})
	}
	others, err := b.otherBoxes(ctx)
	if err != nil {
		return nil, err
	}
	if len(others) > 0 {
		p.warn("custom role %s kept: still used by %v", config.SuspendRoleID, others)
	} else if role, err := b.G.IAM.Projects.Roles.Get(b.C.SuspendRoleName()).Context(ctx).Do(); err == nil && !role.Deleted {
		p.add(Op{Resource: "custom role " + config.SuspendRoleID, Action: "delete", Detail: "undeletable for 7 days; `devbox up` undeletes it", Run: func(ctx context.Context) error {
			_, err := b.G.IAM.Projects.Roles.Delete(b.C.SuspendRoleName()).Context(ctx).Do()
			return err
		}})
	}
	return p, nil
}

// ListSnapshots returns the code disk's snapshots, which carry the box label.
func (b *Box) ListSnapshots(ctx context.Context) ([]*compute.Snapshot, error) {
	var out []*compute.Snapshot
	err := b.G.Compute.Snapshots.List(b.C.ProjectID).Filter(fmt.Sprintf("labels.%s = %q", config.LabelKey, b.C.Name)).
		Pages(ctx, func(l *compute.SnapshotList) error {
			out = append(out, l.Items...)
			return nil
		})
	return out, err
}

// otherBoxes lists instances in the project labelled as some other devbox.
func (b *Box) otherBoxes(ctx context.Context) ([]string, error) {
	var out []string
	err := b.G.Compute.Instances.AggregatedList(b.C.ProjectID).Filter(fmt.Sprintf("labels.%s:*", config.LabelKey)).
		Pages(ctx, func(l *compute.InstanceAggregatedList) error {
			for _, scoped := range l.Items {
				for _, in := range scoped.Instances {
					if v := in.Labels[config.LabelKey]; v != "" && v != b.C.Name {
						out = append(out, v)
					}
				}
			}
			return nil
		})
	return out, err
}
