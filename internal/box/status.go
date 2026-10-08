package box

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/Cool-Consulting-LLC/devbox/internal/gcp"
)

// Monthly on-demand compute for e2-standard-2, from the plan. Verify before trusting.
var computeMonthly = map[string]float64{
	"europe-west3":    63.03,
	"europe-central2": 59.19,
	"me-west1":        53.81,
}

const (
	diskPerGBMonth         = 0.12 // pd-balanced, approximate
	ipMonthly              = 3.65 // ephemeral IPv4 while running
	suspendedRAMPerGBMonth = 0.000233 * 730
	e2Standard2RAMGiB      = 8
)

// Status prints what exists, its state and what it costs.
func (b *Box) Status(ctx context.Context, w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(a ...any) {
		for i, x := range a {
			if i > 0 {
				fmt.Fprint(tw, "\t")
			}
			fmt.Fprint(tw, x)
		}
		fmt.Fprintln(tw)
	}

	in, err := b.GetInstance(ctx)
	if err != nil {
		return err
	}
	state, ip := "absent", ""
	var bootGB int64
	if in != nil {
		state = in.Status
		if len(in.NetworkInterfaces) > 0 && len(in.NetworkInterfaces[0].AccessConfigs) > 0 {
			ip = in.NetworkInterfaces[0].AccessConfigs[0].NatIP
		}
		if bd := bootDisk(in); bd != nil {
			bootGB = bd.DiskSizeGb
		}
		row("instance", b.C.Name, state, gcp.Last(in.MachineType)+" "+ip)
	} else {
		row("instance", b.C.Name, state)
	}

	disk, err := b.getCodeDisk(ctx)
	if err != nil {
		return err
	}
	var codeGB int64
	if disk != nil {
		codeGB = disk.SizeGb
		row("code disk", disk.Name, disk.Status, fmt.Sprintf("%d GB", disk.SizeGb))
	} else {
		row("code disk", b.C.CodeDiskName(), "absent")
	}
	snaps, err := b.ListSnapshots(ctx)
	if err != nil {
		return err
	}
	latest := ""
	for _, s := range snaps {
		if s.CreationTimestamp > latest {
			latest = s.CreationTimestamp
		}
	}
	row("snapshots", fmt.Sprintf("%d", len(snaps)), "", latest)
	if rp, err := b.getSnapshotPolicy(ctx); err != nil {
		return err
	} else if rp != nil {
		row("snapshot schedule", rp.Name, rp.Status)
	} else {
		row("snapshot schedule", b.C.SnapshotPolicyName(), "absent")
	}

	sa, err := b.getSA(ctx)
	if err != nil {
		return err
	}
	row("service account", b.C.SAEmail(), present(sa != nil))
	for _, name := range []string{b.C.FirewallIAPName(), b.C.FirewallGlassName()} {
		fw, err := b.G.Compute.Firewalls.Get(b.C.ProjectID, name).Context(ctx).Do()
		if err != nil && !gcp.IsNotFound(err) {
			return err
		}
		detail := ""
		if fw != nil && err == nil {
			detail = fmt.Sprintf("%s from %v", allowedString(fw.Allowed), fw.SourceRanges)
		}
		row("firewall", name, present(err == nil), detail)
	}
	pols, err := b.listPolicies(ctx)
	if err != nil {
		return err
	}
	chans, err := b.listChannels(ctx)
	if err != nil {
		return err
	}
	row("alerting", fmt.Sprintf("%d policies, %d channels", len(pols), len(chans)))
	secrets, err := b.ListLabelledSecrets(ctx)
	if err != nil {
		return err
	}
	row("secrets", fmt.Sprintf("%d labelled devbox=%s", len(secrets), b.C.Name))
	tw.Flush()

	fmt.Fprintln(w)
	b.printCost(w, state, bootGB, codeGB, ip != "")
	return nil
}

func present(ok bool) string {
	if ok {
		return "present"
	}
	return "absent"
}

func (b *Box) printCost(w io.Writer, state string, bootGB, codeGB int64, hasIP bool) {
	disks := float64(bootGB+codeGB) * diskPerGBMonth
	compute, known := computeMonthly[b.C.Region()]
	known = known && b.C.MachineType == "e2-standard-2"
	var total float64
	var what string
	switch {
	case state == "RUNNING":
		total = disks
		if known {
			total += compute
		}
		if hasIP {
			total += ipMonthly
		}
		what = "running"
	case suspended(state):
		total = disks + suspendedRAMPerGBMonth*e2Standard2RAMGiB
		what = "suspended"
	default:
		total = disks
		what = "at rest"
	}
	if !known && state == "RUNNING" {
		fmt.Fprintf(w, "cost: ~$%.2f/mo for disks and IP while %s; no compute price for %s in %s\n", total, what, b.C.MachineType, b.C.Region())
		return
	}
	fmt.Fprintf(w, "cost: ~$%.2f/mo at the current state (%s), plus snapshots. Estimates; check the billing console.\n", total, what)
}
