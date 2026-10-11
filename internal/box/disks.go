package box

import (
	"context"
	"fmt"
	"slices"
	"strings"

	compute "google.golang.org/api/compute/v1"

	"github.com/Cool-Consulting-LLC/devbox/internal/gcp"
)

const diskType = "pd-balanced"

func (b *Box) getCodeDisk(ctx context.Context) (*compute.Disk, error) {
	d, err := b.G.Compute.Disks.Get(b.C.ProjectID, b.C.Zone, b.C.CodeDiskName()).Context(ctx).Do()
	if gcp.IsNotFound(err) {
		return nil, nil
	}
	if err == nil && !b.ownsLabels(d.Labels) {
		return d, b.notOurs("disk", d.Name, b.labelFix("disks", d.Name+" --zone "+b.C.Zone))
	}
	return d, err
}

// The pet disk: grown, never shrunk, never deleted without destroy --all.
func (b *Box) planCodeDisk(ctx context.Context, p *Plan) error {
	d, err := b.getCodeDisk(ctx)
	if err != nil {
		return fmt.Errorf("code disk: %w", err)
	}
	res := "disk " + b.C.CodeDiskName()
	switch {
	case d == nil:
		p.add(Op{Resource: res, Action: "create", Detail: fmt.Sprintf("%d GB %s, mounted at /srv/code", b.C.CodeDiskGB, diskType), Run: func(ctx context.Context) error {
			return b.G.Await(ctx)(b.G.Compute.Disks.Insert(b.C.ProjectID, b.C.Zone, &compute.Disk{
				Name:        b.C.CodeDiskName(),
				Description: "devbox " + b.C.Name + ": /srv/code. Everything durable lives here.",
				SizeGb:      b.C.CodeDiskGB,
				Type:        fmt.Sprintf("zones/%s/diskTypes/%s", b.C.Zone, diskType),
				Labels:      b.C.Labels(),
			}).Context(ctx).Do())
		}})
	case d.SizeGb > b.C.CodeDiskGB:
		p.warn("code_disk_gb is %d but %s is %d GB; disks only grow, so it stays %d GB", b.C.CodeDiskGB, b.C.CodeDiskName(), d.SizeGb, d.SizeGb)
	case d.SizeGb < b.C.CodeDiskGB:
		p.NeedsReprovision = true
		p.add(Op{Resource: res, Action: "update", Detail: fmt.Sprintf("grow %d -> %d GB (reprovision grows the filesystem)", d.SizeGb, b.C.CodeDiskGB), Run: func(ctx context.Context) error {
			return b.G.Await(ctx)(b.G.Compute.Disks.Resize(b.C.ProjectID, b.C.Zone, b.C.CodeDiskName(),
				&compute.DisksResizeRequest{SizeGb: b.C.CodeDiskGB}).Context(ctx).Do())
		}})
	}
	return nil
}

func (b *Box) desiredSnapshotPolicy() *compute.ResourcePolicy {
	return &compute.ResourcePolicy{
		Name:        b.C.SnapshotPolicyName(),
		Description: "devbox " + b.C.Name + ": daily snapshots of /srv/code. Managed by the devbox binary.",
		SnapshotSchedulePolicy: &compute.ResourcePolicySnapshotSchedulePolicy{
			Schedule: &compute.ResourcePolicySnapshotSchedulePolicySchedule{
				DailySchedule: &compute.ResourcePolicyDailyCycle{DaysInCycle: 1, StartTime: b.C.Snapshots.StartTime},
			},
			RetentionPolicy: &compute.ResourcePolicySnapshotSchedulePolicyRetentionPolicy{
				MaxRetentionDays:   b.C.Snapshots.RetentionDays,
				OnSourceDiskDelete: "KEEP_AUTO_SNAPSHOTS",
			},
			SnapshotProperties: &compute.ResourcePolicySnapshotSchedulePolicySnapshotProperties{
				Labels: b.C.Labels(),
			},
		},
	}
}

func (b *Box) getSnapshotPolicy(ctx context.Context) (*compute.ResourcePolicy, error) {
	rp, err := b.G.Compute.ResourcePolicies.Get(b.C.ProjectID, b.C.Region(), b.C.SnapshotPolicyName()).Context(ctx).Do()
	if gcp.IsNotFound(err) {
		return nil, nil
	}
	if err == nil && !strings.HasPrefix(rp.Description, b.managedDesc()) {
		return rp, b.notOurs("snapshot schedule", rp.Name, "delete it if it is a leftover")
	}
	return rp, err
}

// Resource policies take no labels, so the schedule is found by name.
func (b *Box) planSnapshots(ctx context.Context, p *Plan) error {
	rp, err := b.getSnapshotPolicy(ctx)
	if err != nil {
		return fmt.Errorf("snapshot schedule: %w", err)
	}
	want := b.desiredSnapshotPolicy()
	res := "snapshot schedule " + want.Name
	if rp == nil {
		p.add(Op{Resource: res, Action: "create", Detail: fmt.Sprintf("daily %s UTC, keep %d days", b.C.Snapshots.StartTime, b.C.Snapshots.RetentionDays), Run: func(ctx context.Context) error {
			return b.G.Await(ctx)(b.G.Compute.ResourcePolicies.Insert(b.C.ProjectID, b.C.Region(), want).Context(ctx).Do())
		}})
	} else if d := snapshotDiff(rp, want); d != "" {
		p.add(Op{Resource: res, Action: "update", Detail: d, Run: func(ctx context.Context) error {
			patch := &compute.ResourcePolicy{SnapshotSchedulePolicy: want.SnapshotSchedulePolicy}
			return b.G.Await(ctx)(b.G.Compute.ResourcePolicies.Patch(b.C.ProjectID, b.C.Region(), want.Name, patch).Context(ctx).Do())
		}})
	}

	disk, err := b.getCodeDisk(ctx)
	if err != nil {
		return err
	}
	if disk != nil && slices.ContainsFunc(disk.ResourcePolicies, func(u string) bool { return gcp.Last(u) == want.Name }) {
		return nil
	}
	p.add(Op{Resource: res, Action: "update", Detail: "attach to " + b.C.CodeDiskName(), Run: func(ctx context.Context) error {
		policy := fmt.Sprintf("projects/%s/regions/%s/resourcePolicies/%s", b.C.ProjectID, b.C.Region(), want.Name)
		return b.G.Await(ctx)(b.G.Compute.Disks.AddResourcePolicies(b.C.ProjectID, b.C.Zone, b.C.CodeDiskName(),
			&compute.DisksAddResourcePoliciesRequest{ResourcePolicies: []string{policy}}).Context(ctx).Do())
	}})
	return nil
}

func snapshotDiff(have, want *compute.ResourcePolicy) string {
	h, w := have.SnapshotSchedulePolicy, want.SnapshotSchedulePolicy
	if h == nil || h.Schedule == nil || h.Schedule.DailySchedule == nil || h.RetentionPolicy == nil {
		return "schedule"
	}
	var d string
	if h.Schedule.DailySchedule.StartTime != w.Schedule.DailySchedule.StartTime {
		d += fmt.Sprintf("start %s -> %s ", h.Schedule.DailySchedule.StartTime, w.Schedule.DailySchedule.StartTime)
	}
	if h.RetentionPolicy.MaxRetentionDays != w.RetentionPolicy.MaxRetentionDays {
		d += fmt.Sprintf("retention %d -> %d days ", h.RetentionPolicy.MaxRetentionDays, w.RetentionPolicy.MaxRetentionDays)
	}
	if h.RetentionPolicy.OnSourceDiskDelete != w.RetentionPolicy.OnSourceDiskDelete {
		d += "keep snapshots on disk delete "
	}
	return d
}
