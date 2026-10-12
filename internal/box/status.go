package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	monitoring "google.golang.org/api/monitoring/v3"

	"github.com/Cool-Consulting-LLC/devbox/internal/gcp"
)

// Monthly on-demand compute for e2-standard-2, from the plan. Verify before trusting.
// Other e2-standard sizes scale linearly: e2-standard-N costs N/2 times this.
var computeMonthly = map[string]float64{
	"europe-west3":    63.03,
	"europe-west4":    53.85,
	"europe-central2": 59.19,
	"me-west1":        53.81,
}

const (
	diskPerGBMonth         = 0.12 // pd-balanced, approximate
	ipMonthly              = 3.65 // ephemeral IPv4 while running
	suspendedRAMPerGBMonth = 0.000233 * 730
)

// e2StandardVCPUs returns N for e2-standard-N, which has 4N GiB of memory.
func e2StandardVCPUs(machineType string) (int, bool) {
	var n int
	if _, err := fmt.Sscanf(machineType, "e2-standard-%d", &n); err != nil || machineType != fmt.Sprintf("e2-standard-%d", n) {
		return 0, false
	}
	return n, true
}

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
	if errors.Is(err, ErrNotOurs) {
		row("instance", b.C.Name, "NOT DEVBOX'S", "a VM by this name exists without the devbox="+b.C.Name+" label")
		in = nil
	} else if err != nil {
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
	} else if err == nil {
		row("instance", b.C.Name, state)
	}

	disk, err := b.getCodeDisk(ctx)
	if err != nil && !errors.Is(err, ErrNotOurs) {
		return err
	}
	var codeGB int64
	if errors.Is(err, ErrNotOurs) {
		row("code disk", disk.Name, "NOT DEVBOX'S", "no devbox="+b.C.Name+" label")
	} else if disk != nil {
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
	if rp, err := b.getSnapshotPolicy(ctx); errors.Is(err, ErrNotOurs) {
		row("snapshot schedule", rp.Name, "NOT DEVBOX'S")
	} else if err != nil {
		return err
	} else if rp != nil {
		row("snapshot schedule", rp.Name, rp.Status)
	} else {
		row("snapshot schedule", b.C.SnapshotPolicyName(), "absent")
	}

	sa, err := b.getSA(ctx)
	if errors.Is(err, ErrNotOurs) {
		row("service account", b.C.SAEmail(), "NOT DEVBOX'S")
	} else if err != nil {
		return err
	} else {
		row("service account", b.C.SAEmail(), present(sa != nil))
	}
	for _, name := range []string{b.C.FirewallIAPName(), b.C.FirewallGlassName()} {
		fw, err := b.getFirewall(ctx, name)
		if errors.Is(err, ErrNotOurs) {
			row("firewall", name, "NOT DEVBOX'S")
			continue
		} else if err != nil {
			return err
		}
		detail := ""
		if fw != nil {
			detail = fmt.Sprintf("%s from %v", allowedString(fw.Allowed), fw.SourceRanges)
		}
		row("firewall", name, present(fw != nil), detail)
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
	if bootGB == 0 {
		bootGB = b.C.BootDiskGB
	}
	if codeGB == 0 {
		codeGB = b.C.CodeDiskGB
	}
	ci := costInputs{bootGB: bootGB, codeGB: codeGB, now: time.Now()}
	if in != nil {
		ci.runHours, ci.runErr = b.uptimeHours(ctx, monthStart(ci.now), ci.now)
	}
	b.printCost(w, ci)
	return nil
}

func present(ok bool) string {
	if ok {
		return "present"
	}
	return "absent"
}

// costInputs is what the cost table needs from the live resources.
type costInputs struct {
	bootGB, codeGB int64
	now            time.Time
	runHours       float64 // hours running so far this month
	runErr         error   // why runHours is unknown
}

// uptimeHours sums the instance's uptime metric over [start, end). Suspended and
// stopped time do not count; a recreated instance's earlier uptime does.
func (b *Box) uptimeHours(ctx context.Context, start, end time.Time) (float64, error) {
	filter := fmt.Sprintf(`metric.type="compute.googleapis.com/instance/uptime" AND metric.labels.instance_name=%q AND resource.labels.zone=%q`, b.C.Name, b.C.Zone)
	var secs float64
	err := b.G.Monitoring.Projects.TimeSeries.List("projects/"+b.C.ProjectID).
		Filter(filter).
		IntervalStartTime(start.Format(time.RFC3339)).
		IntervalEndTime(end.Format(time.RFC3339)).
		AggregationAlignmentPeriod("3600s").
		AggregationPerSeriesAligner("ALIGN_SUM").
		Pages(ctx, func(r *monitoring.ListTimeSeriesResponse) error {
			for _, ts := range r.TimeSeries {
				for _, p := range ts.Points {
					if p.Value != nil && p.Value.DoubleValue != nil {
						secs += *p.Value.DoubleValue
					}
				}
			}
			return nil
		})
	return secs / 3600, err
}

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// printCost prints a small table. Monthly rates assume 730 hours; this month's
// figures use the uptime metric for running hours and count every other hour as
// suspended. The month is UTC, which is close to but not exactly the invoice month.
func (b *Box) printCost(w io.Writer, in costInputs) {
	disks := float64(in.bootGB+in.codeGB) * diskPerGBMonth
	compute, known := computeMonthly[b.C.Region()]
	vcpus, standard := e2StandardVCPUs(b.C.MachineType)
	known = known && standard
	compute *= float64(vcpus) / 2
	ip := 0.0
	if b.C.WantExternalIP() {
		ip = ipMonthly
	}
	suspendedRAM := suspendedRAMPerGBMonth * float64(4*vcpus)
	running := disks + compute + ip
	idle := disks + suspendedRAM

	start := monthStart(in.now)
	monthHours := start.AddDate(0, 1, 0).Sub(start).Hours()
	elapsed := in.now.Sub(start).Hours()
	runH := min(in.runHours, elapsed)
	toDate := (runH*running + (elapsed-runH)*idle) / 730

	money := func(v float64) string { return fmt.Sprintf("$%.2f", v) }
	toDateS, projS, runS := money(toDate), "", money(running)
	if elapsed > 0 {
		projS = money(toDate * monthHours / elapsed)
	}
	note := fmt.Sprintf("%.1f h running", runH)
	switch {
	case !known:
		toDateS, projS, runS = "?", "?", "?"
		note = fmt.Sprintf("no compute price for %s in %s", b.C.MachineType, b.C.Region())
	case in.runErr != nil:
		toDateS, projS = "?", "?"
		note = "no uptime data: " + in.runErr.Error()
	}

	fmt.Fprintln(w, "cost (estimates, excluding snapshots; check the billing console):")
	line := func(label, amount, note string) { fmt.Fprintf(w, "  %-22s%9s  %s\n", label, amount, note) }
	line("this month to date", toDateS, note)
	line("this month projected", projS, "at this month's pace")
	line("per month suspended", money(idle), fmt.Sprintf("disks %s, suspended memory %s", money(disks), money(suspendedRAM)))
	if known {
		line("per month running", runS, fmt.Sprintf("disks %s, %s %s, IP %s", money(disks), b.C.MachineType, money(compute), money(ip)))
	} else {
		line("per month running", runS, fmt.Sprintf("disks %s, IP %s, plus compute", money(disks), money(ip)))
	}
}
