package box

import (
	"context"
	"fmt"
	"slices"

	monitoring "google.golang.org/api/monitoring/v3"

	"github.com/Cool-Consulting-LLC/devbox/internal/config"
)

// A suspended box reports no metrics. INACTIVE closes open incidents and opens
// none; NO_OP leaves them open for a week and ACTIVE fires on every suspend.
const missingData = "EVALUATION_MISSING_DATA_INACTIVE"

type alertSpec struct {
	key, title, doc string
	filter          string
	cmp             string
	threshold       float64
	duration        string
}

func (b *Box) alertSpecs(instanceID uint64) []alertSpec {
	id := fmt.Sprint(instanceID)
	cpu := fmt.Sprintf(`resource.type = "gce_instance" AND resource.labels.instance_id = "%s" AND metric.type = "compute.googleapis.com/instance/cpu/utilization"`, id)
	// The code disk is the instance's second SCSI disk. See the sharp-edges table.
	disk := fmt.Sprintf(`resource.type = "gce_instance" AND resource.labels.instance_id = "%s" AND metric.type = "agent.googleapis.com/disk/percent_used" AND metric.labels.state = "used" AND metric.labels.device = monitoring.regex.full_match("(/dev/)?sdb1?")`, id)
	a := b.C.Alerts
	return []alertSpec{
		{"cpu-high", "CPU high", "The box has been above the CPU threshold for 10 minutes. It is undersized for what you are doing.",
			cpu, "COMPARISON_GT", float64(a.CPUHighPct) / 100, "600s"},
		{"cpu-idle", "CPU idle for 6 hours without suspending", "A running box has been idle for 6 hours. With idle suspend on, either something kept the box busy or the idle timer is broken. On the box, `cat /run/devbox-idle.status` shows what the last check counted (keyboard input, Claude Code turns, non-interactive SSH); `systemctl status devbox-idle.timer` shows whether it runs.",
			cpu, "COMPARISON_LT", float64(a.CPUIdlePct) / 100, "21600s"},
		{"disk-full", "/srv/code filling up", "The code disk is above the threshold. Grow it with code_disk_gb and `devbox apply`, then `devbox reprovision`.",
			disk, "COMPARISON_GT", float64(a.DiskUsedPct), "300s"},
	}
}

func (b *Box) policyName(s alertSpec) string { return fmt.Sprintf("%s: %s", b.C.Name, s.title) }

func (b *Box) desiredPolicy(s alertSpec, channel string) *monitoring.AlertPolicy {
	return &monitoring.AlertPolicy{
		DisplayName:          b.policyName(s),
		Combiner:             "OR",
		Enabled:              true,
		NotificationChannels: []string{channel},
		UserLabels:           map[string]string{config.LabelKey: b.C.Name, "devbox-alert": s.key},
		Documentation:        &monitoring.Documentation{Content: s.doc, MimeType: "text/markdown"},
		Conditions: []*monitoring.Condition{{
			DisplayName: s.title,
			ConditionThreshold: &monitoring.MetricThreshold{
				Filter:                s.filter,
				Comparison:            s.cmp,
				ThresholdValue:        s.threshold,
				Duration:              s.duration,
				EvaluationMissingData: missingData,
				Aggregations: []*monitoring.Aggregation{{
					AlignmentPeriod: "60s", PerSeriesAligner: "ALIGN_MEAN",
				}},
				Trigger: &monitoring.Trigger{Count: 1},
			},
		}},
		AlertStrategy: &monitoring.AlertStrategy{AutoClose: "86400s"},
	}
}

func policyDiff(have, want *monitoring.AlertPolicy) bool {
	if len(have.Conditions) != 1 || have.Conditions[0].ConditionThreshold == nil {
		return true
	}
	h, w := have.Conditions[0].ConditionThreshold, want.Conditions[0].ConditionThreshold
	return have.DisplayName != want.DisplayName || have.Documentation == nil || have.Documentation.Content != want.Documentation.Content ||
		h.Filter != w.Filter || h.Comparison != w.Comparison || h.ThresholdValue != w.ThresholdValue ||
		h.Duration != w.Duration || h.EvaluationMissingData != w.EvaluationMissingData ||
		!slices.Equal(have.NotificationChannels, want.NotificationChannels) ||
		!have.Enabled
}

func (b *Box) listChannels(ctx context.Context) ([]*monitoring.NotificationChannel, error) {
	var out []*monitoring.NotificationChannel
	err := b.G.Monitoring.Projects.NotificationChannels.List("projects/"+b.C.ProjectID).Pages(ctx, func(r *monitoring.ListNotificationChannelsResponse) error {
		for _, c := range r.NotificationChannels {
			if c.UserLabels[config.LabelKey] == b.C.Name {
				out = append(out, c)
			}
		}
		return nil
	})
	return out, err
}

func (b *Box) listPolicies(ctx context.Context) ([]*monitoring.AlertPolicy, error) {
	var out []*monitoring.AlertPolicy
	err := b.G.Monitoring.Projects.AlertPolicies.List("projects/"+b.C.ProjectID).Pages(ctx, func(r *monitoring.ListAlertPoliciesResponse) error {
		for _, p := range r.AlertPolicies {
			if p.UserLabels[config.LabelKey] == b.C.Name {
				out = append(out, p)
			}
		}
		return nil
	})
	return out, err
}

func (b *Box) planAlerting(ctx context.Context, p *Plan) error {
	chans, err := b.listChannels(ctx)
	if err != nil {
		return fmt.Errorf("notification channels: %w", err)
	}
	pols, err := b.listPolicies(ctx)
	if err != nil {
		return fmt.Errorf("alert policies: %w", err)
	}

	if b.C.Alerts.Email == "" {
		for _, pol := range pols {
			p.add(b.deletePolicyOp(pol))
		}
		for _, c := range chans {
			p.add(b.deleteChannelOp(c))
		}
		return nil
	}

	if len(chans) == 0 {
		p.add(Op{Resource: "notification channel", Action: "create", Detail: "email " + b.C.Alerts.Email, Run: func(ctx context.Context) error {
			_, err := b.ensureChannel(ctx)
			return err
		}})
	} else if chans[0].Labels["email_address"] != b.C.Alerts.Email {
		p.add(Op{Resource: "notification channel", Action: "update", Detail: chans[0].Labels["email_address"] + " -> " + b.C.Alerts.Email, Run: func(ctx context.Context) error {
			_, err := b.ensureChannel(ctx)
			return err
		}})
	}
	for _, c := range chans[min(1, len(chans)):] {
		p.add(b.deleteChannelOp(c))
	}

	// The policies pin the instance's numeric ID, which a replacement changes.
	in, err := b.GetInstance(ctx)
	if err != nil {
		return err
	}
	byKey := map[string]*monitoring.AlertPolicy{}
	for _, pol := range pols {
		k := pol.UserLabels["devbox-alert"]
		if _, dup := byKey[k]; dup || k == "" {
			p.add(b.deletePolicyOp(pol))
			continue
		}
		byKey[k] = pol
	}
	channel := ""
	if len(chans) > 0 {
		channel = chans[0].Name
	}
	// Rendered again at run time when the instance or channel is about to change.
	pending := in == nil || channel == "" || chans[0].Labels["email_address"] != b.C.Alerts.Email || b.instanceWillChange(p)
	var id uint64
	if in != nil {
		id = in.Id
	}
	for _, s := range b.alertSpecs(id) {
		have := byKey[s.key]
		delete(byKey, s.key)
		if !pending && have != nil && !policyDiff(have, b.desiredPolicy(s, channel)) {
			continue
		}
		action := "update"
		if have == nil {
			action = "create"
		}
		p.add(Op{Resource: "alert policy " + s.key, Action: action, Detail: alertDetail(s), Run: func(ctx context.Context) error {
			return b.ensurePolicy(ctx, s.key)
		}})
	}
	for _, pol := range byKey {
		p.add(b.deletePolicyOp(pol))
	}
	return nil
}

func (b *Box) instanceWillChange(p *Plan) bool {
	for _, o := range p.Ops {
		if o.Resource == "instance "+b.C.Name && (o.Action == "create" || o.Action == "replace") {
			return true
		}
	}
	return false
}

func alertDetail(s alertSpec) string {
	op := map[string]string{"COMPARISON_GT": ">", "COMPARISON_LT": "<"}[s.cmp]
	v := s.threshold
	if s.key != "disk-full" {
		v *= 100
	}
	return fmt.Sprintf("%s %g%% for %s", op, v, s.duration)
}

func (b *Box) ensureChannel(ctx context.Context) (string, error) {
	chans, err := b.listChannels(ctx)
	if err != nil {
		return "", err
	}
	want := &monitoring.NotificationChannel{
		Type:        "email",
		DisplayName: "devbox " + b.C.Name,
		Description: "Managed by the devbox binary.",
		Labels:      map[string]string{"email_address": b.C.Alerts.Email},
		UserLabels:  b.C.Labels(),
		Enabled:     true,
	}
	if len(chans) == 0 {
		c, err := b.G.Monitoring.Projects.NotificationChannels.Create("projects/"+b.C.ProjectID, want).Context(ctx).Do()
		if err != nil {
			return "", err
		}
		return c.Name, nil
	}
	c := chans[0]
	if c.Labels["email_address"] != b.C.Alerts.Email {
		if _, err := b.G.Monitoring.Projects.NotificationChannels.Patch(c.Name, want).UpdateMask("labels").Context(ctx).Do(); err != nil {
			return "", err
		}
	}
	return c.Name, nil
}

func (b *Box) ensurePolicy(ctx context.Context, key string) error {
	channel, err := b.ensureChannel(ctx)
	if err != nil {
		return err
	}
	in, err := b.GetInstance(ctx)
	if err != nil {
		return err
	}
	if in == nil {
		return fmt.Errorf("instance %s does not exist yet", b.C.Name)
	}
	var spec alertSpec
	for _, s := range b.alertSpecs(in.Id) {
		if s.key == key {
			spec = s
		}
	}
	want := b.desiredPolicy(spec, channel)
	pols, err := b.listPolicies(ctx)
	if err != nil {
		return err
	}
	for _, have := range pols {
		if have.UserLabels["devbox-alert"] == key {
			_, err := b.G.Monitoring.Projects.AlertPolicies.Patch(have.Name, want).Context(ctx).Do()
			return err
		}
	}
	_, err = b.G.Monitoring.Projects.AlertPolicies.Create("projects/"+b.C.ProjectID, want).Context(ctx).Do()
	return err
}

func (b *Box) deletePolicyOp(pol *monitoring.AlertPolicy) Op {
	return Op{Resource: "alert policy " + pol.DisplayName, Action: "delete", Run: func(ctx context.Context) error {
		_, err := b.G.Monitoring.Projects.AlertPolicies.Delete(pol.Name).Context(ctx).Do()
		return err
	}}
}

func (b *Box) deleteChannelOp(c *monitoring.NotificationChannel) Op {
	return Op{Resource: "notification channel " + c.Labels["email_address"], Action: "delete", Run: func(ctx context.Context) error {
		_, err := b.G.Monitoring.Projects.NotificationChannels.Delete(c.Name).Force(true).Context(ctx).Do()
		return err
	}}
}
