package box

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	compute "google.golang.org/api/compute/v1"

	"github.com/Cool-Consulting-LLC/devbox/internal/config"
	"github.com/Cool-Consulting-LLC/devbox/internal/gcp"
)

type env struct {
	t   *testing.T
	f   *fakeGCP
	b   *Box
	ctx context.Context
}

func setup(t *testing.T) *env {
	f := newFake(t)
	f.addSecret("devbox-tailscale-oauth", "devbox", 1)
	f.addSecret("devbox-ssh-key", "devbox", 1)
	ctx := context.Background()
	cl, err := gcp.NewForTest(ctx, "my-devbox-project", "me@example.com", f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := testBox(t).C
	return &env{t: t, f: f, b: New(c, cl, io.Discard), ctx: ctx}
}

func (e *env) plan(mode Mode) *Plan {
	e.t.Helper()
	p, err := e.b.Reconcile(e.ctx, mode)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) run(mode Mode) *Plan {
	e.t.Helper()
	p := e.plan(mode)
	if err := e.b.Execute(e.ctx, p.Ops); err != nil {
		e.t.Fatalf("execute: %v\nplan:\n%s", err, opsString(p))
	}
	return p
}

func (e *env) assertConverged() {
	e.t.Helper()
	if p := e.plan(ModeApply); !p.Empty() {
		e.t.Fatalf("not converged; apply would still do:\n%s", opsString(p))
	}
}

func opsString(p *Plan) string {
	var b strings.Builder
	p.Print(&b)
	return b.String()
}

func actions(p *Plan) []string {
	var out []string
	for _, o := range p.Ops {
		out = append(out, o.Action+" "+o.Resource)
	}
	return out
}

func TestUpFromNothing(t *testing.T) {
	e := setup(t)
	p := e.run(ModeUp)
	if len(p.Warnings) != 0 {
		t.Errorf("warnings: %v", p.Warnings)
	}
	f := e.f
	in := f.instances["devbox"]
	if in == nil || in.Status != "RUNNING" {
		t.Fatal("no running instance")
	}
	if f.sas[e.b.C.SAEmail()] == nil || f.roles[config.SuspendRoleID] == nil {
		t.Fatal("service account or role missing")
	}
	if f.firewalls["devbox-allow-iap-ssh"] == nil || f.firewalls["devbox-break-glass"] != nil {
		t.Fatal("firewalls wrong")
	}
	cd := f.disks["devbox-code"]
	if cd == nil || len(cd.ResourcePolicies) != 1 || !slices.Equal(cd.Users, []string{"devbox"}) {
		t.Fatalf("code disk: %+v", cd)
	}
	if hasMember(f.projPolicy, secretRole, e.b.C.SAMember(), nil) {
		t.Fatal("unconditional secretAccessor granted")
	}
	found := false
	for _, bd := range f.projPolicy.Bindings {
		if bd.Role == secretRole {
			if bd.Condition == nil || !strings.Contains(bd.Condition.Expression, `"projects/123456789/secrets/devbox-"`) {
				t.Fatalf("secretAccessor must be conditioned on the project number: %+v", bd.Condition)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no secretAccessor binding")
	}
	if pol := f.instIAM["devbox"]; pol == nil || pol.Bindings[0].Role != e.b.C.SuspendRoleName() {
		t.Fatal("self-suspend not granted on the instance")
	}
	if len(f.channels) != 1 || len(f.alerts) != 3 {
		t.Fatalf("alerting: %d channels %d policies", len(f.channels), len(f.alerts))
	}
	for _, a := range f.alerts {
		if !strings.Contains(a.Conditions[0].ConditionThreshold.Filter, `instance_id = "`+fmt.Sprint(in.Id)+`"`) {
			t.Errorf("%s not pinned to instance %d", a.DisplayName, in.Id)
		}
	}
	e.assertConverged()
}

// Rewording an alert reaches policies created before the change.
func TestAlertWordingUpdates(t *testing.T) {
	e := setup(t)
	e.run(ModeUp)
	for _, a := range e.f.alerts {
		if a.UserLabels["devbox-alert"] == "cpu-idle" {
			a.DisplayName = "devbox: CPU idle (idle suspend failed)"
		}
	}
	p := e.run(ModeApply)
	if got := actions(p); !slices.Equal(got, []string{"update alert policy cpu-idle"}) {
		t.Fatalf("reworded alert: %v", got)
	}
	e.assertConverged()
}

func TestMissingSecretsWarn(t *testing.T) {
	e := setup(t)
	delete(e.f.secrets, "devbox-ssh-key")
	p := e.plan(ModeUp)
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "devbox-ssh-key") {
		t.Fatalf("warnings: %v", p.Warnings)
	}
}

func TestSuspendedBox(t *testing.T) {
	e := setup(t)
	e.run(ModeUp)
	e.f.instances["devbox"].Status = "SUSPENDED"

	// apply with nothing to change leaves a suspended box alone.
	e.assertConverged()

	// up resumes it, and does nothing else.
	p := e.run(ModeUp)
	if got := actions(p); !slices.Equal(got, []string{"resume instance devbox"}) {
		t.Fatalf("up on a suspended box did %v", got)
	}
	if e.f.instances["devbox"].Status != "RUNNING" {
		t.Fatal("not resumed")
	}
}

func TestMachineTypeChangeOnSuspendedBox(t *testing.T) {
	e := setup(t)
	e.run(ModeUp)
	e.f.instances["devbox"].Status = "SUSPENDED"
	e.b.C.MachineType = "e2-standard-4"

	if p := e.plan(ModeUp); len(p.Disruptive()) != 0 || len(p.Warnings) == 0 {
		t.Fatalf("up must skip the restart and say so: %s", opsString(p))
	}
	mark := e.f.mark()
	p := e.run(ModeApply)
	if got := actions(p); !slices.Equal(got, []string{"resume instance devbox", "restart instance devbox"}) {
		t.Fatalf("apply did %v", got)
	}
	calls := strings.Join(e.f.callsSince(mark), "\n")
	for _, seq := range [][2]string{{"/resume", "/stop"}, {"/stop", "/setMachineType"}, {"/setMachineType", "/start"}} {
		if i, j := strings.Index(calls, seq[0]), strings.Index(calls, seq[1]); i < 0 || j < 0 || i > j {
			t.Fatalf("%s must precede %s:\n%s", seq[0], seq[1], calls)
		}
	}
	in := e.f.instances["devbox"]
	if in.Status != "RUNNING" || gcp.Last(in.MachineType) != "e2-standard-4" {
		t.Fatalf("instance %s %s", in.Status, in.MachineType)
	}
	e.assertConverged()
}

func TestCloudInitChangeReplaces(t *testing.T) {
	e := setup(t)
	e.run(ModeUp)
	old := e.f.instances["devbox"]
	oldID := old.Id
	for _, it := range old.Metadata.Items {
		if it.Key == mdUserData {
			v := "#cloud-config\n# an older cloud-init\n"
			it.Value = &v
		}
	}

	if p := e.plan(ModeUp); len(p.Disruptive()) != 0 {
		t.Fatal("up must never replace")
	}
	p := e.plan(ModeApply)
	if d := p.Disruptive(); len(d) != 1 || d[0].Action != "replace" {
		t.Fatalf("apply should replace:\n%s", opsString(p))
	}
	e.run(ModeApply)
	in := e.f.instances["devbox"]
	if in.Id == oldID {
		t.Fatal("instance not recreated")
	}
	if cd := e.f.disks["devbox-code"]; cd == nil || !slices.Equal(cd.Users, []string{"devbox"}) {
		t.Fatal("code disk lost or not reattached")
	}
	for _, a := range e.f.alerts {
		if !strings.Contains(a.Conditions[0].ConditionThreshold.Filter, `"`+fmt.Sprint(in.Id)+`"`) {
			t.Errorf("%s still pinned to the old instance", a.DisplayName)
		}
	}
	if pol := e.f.instIAM["devbox"]; pol == nil {
		t.Error("self-suspend grant not restored on the new instance")
	}
	e.assertConverged()
}

func TestReprovisionTierChangeIsInPlace(t *testing.T) {
	e := setup(t)
	e.run(ModeUp)
	userData := metadataMap(e.f.instances["devbox"].Metadata)[mdUserData]
	e.b.C.Git.Email = "new@example.com"
	e.b.C.CodeDiskGB = 80

	p := e.run(ModeUp)
	if !p.NeedsReprovision {
		t.Fatal("should ask for reprovision")
	}
	md := metadataMap(e.f.instances["devbox"].Metadata)
	if !strings.Contains(md[mdConfig], "new@example.com") || md[mdUserData] != userData {
		t.Fatal("config not updated, or user-data touched")
	}
	if e.f.disks["devbox-code"].SizeGb != 80 {
		t.Fatal("code disk not grown")
	}
	e.b.C.CodeDiskGB = 40
	if p := e.plan(ModeApply); !p.Empty() || len(p.Warnings) == 0 {
		t.Fatalf("shrink must be a warning, not an op: %s", opsString(p))
	}
}

func TestSyncProvisionMetadataNeverTouchesUserData(t *testing.T) {
	e := setup(t)
	e.run(ModeUp)
	for _, it := range e.f.instances["devbox"].Metadata.Items {
		if it.Key == mdUserData {
			v := "#cloud-config\n# older\n"
			it.Value = &v
		}
	}
	e.b.C.Git.Name = "Someone Else"
	if _, err := e.b.SyncProvisionMetadata(e.ctx); err != nil {
		t.Fatal(err)
	}
	md := metadataMap(e.f.instances["devbox"].Metadata)
	if md[mdUserData] != "#cloud-config\n# older\n" {
		t.Fatal("reprovision rewrote user-data; apply would no longer see the replacement")
	}
	if !strings.Contains(md[mdConfig], "Someone Else") {
		t.Fatal("config not pushed")
	}
}

func TestBreakGlassToggle(t *testing.T) {
	e := setup(t)
	e.run(ModeUp)
	e.b.C.Glass = config.BreakGlass{Enabled: true, SourceCIDR: "203.0.113.7/32"}
	e.run(ModeApply)
	fw := e.f.firewalls["devbox-break-glass"]
	if fw == nil || fw.SourceRanges[0] != "203.0.113.7/32" || fw.TargetServiceAccounts[0] != e.b.C.SAEmail() {
		t.Fatalf("break glass rule: %+v", fw)
	}
	e.b.C.Glass.AllowMosh = true
	p := e.run(ModeApply)
	if got := actions(p); !slices.Equal(got, []string{"update firewall devbox-break-glass"}) {
		t.Fatalf("%v", got)
	}
	e.b.C.Glass.Enabled = false
	e.run(ModeApply)
	if e.f.firewalls["devbox-break-glass"] != nil {
		t.Fatal("not removed")
	}
	e.assertConverged()
}

func TestExternalIPToggle(t *testing.T) {
	e := setup(t)
	e.run(ModeUp)
	e.b.C.ExternalIP = new(bool)
	e.run(ModeApply)
	if len(e.f.instances["devbox"].NetworkInterfaces[0].AccessConfigs) != 0 {
		t.Fatal("IP not removed")
	}
	e.assertConverged()
}

func TestAlertsChangeAndDisable(t *testing.T) {
	e := setup(t)
	e.run(ModeUp)
	e.b.C.Alerts.CPUHighPct = 90
	e.b.C.Alerts.Email = "other@example.com"
	e.run(ModeApply)
	for _, c := range e.f.channels {
		if c.Labels["email_address"] != "other@example.com" {
			t.Fatal("channel email not updated")
		}
	}
	e.assertConverged()

	e.b.C.Alerts.Email = ""
	e.run(ModeApply)
	if len(e.f.alerts) != 0 || len(e.f.channels) != 0 {
		t.Fatal("alerting not removed")
	}
	e.assertConverged()
}

func TestDestroyKeepsPetsAndUpReattaches(t *testing.T) {
	e := setup(t)
	e.run(ModeUp)
	p, err := e.b.PlanDestroy(e.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.b.Execute(e.ctx, p.Ops); err != nil {
		t.Fatalf("%v\n%s", err, opsString(p))
	}
	f := e.f
	if len(f.instances) != 0 || len(f.sas) != 0 || len(f.firewalls) != 0 || len(f.policies) != 0 || len(f.alerts) != 0 || len(f.channels) != 0 {
		t.Fatal("something left behind")
	}
	if f.disks["devbox-code"] == nil || len(f.secrets) != 2 || f.roles[config.SuspendRoleID].Deleted {
		t.Fatal("a pet was destroyed")
	}
	if len(f.disks["devbox-code"].ResourcePolicies) != 0 {
		t.Fatal("schedule still attached")
	}
	for _, bd := range f.projPolicy.Bindings {
		for _, m := range bd.Members {
			if strings.Contains(m, e.b.C.SAEmail()) {
				t.Fatalf("binding left: %s %s", bd.Role, m)
			}
		}
	}

	p = e.run(ModeUp)
	for _, a := range actions(p) {
		if a == "create disk devbox-code" {
			t.Fatal("up recreated the code disk instead of reusing it")
		}
	}
	e.assertConverged()
}

func TestDestroyAll(t *testing.T) {
	e := setup(t)
	e.run(ModeUp)
	e.f.snapshots["s1"] = &compute.Snapshot{Name: "s1", Labels: map[string]string{"devbox": "devbox"}}
	e.f.snapshots["other"] = &compute.Snapshot{Name: "other", Labels: map[string]string{"devbox": "devbox-test"}}
	e.f.addSecret("devbox-unrelated", "devbox-test", 1)

	p, err := e.b.PlanDestroy(e.ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.b.Execute(e.ctx, p.Ops); err != nil {
		t.Fatalf("%v\n%s", err, opsString(p))
	}
	f := e.f
	if len(f.disks) != 0 || f.snapshots["s1"] != nil || f.snapshots["other"] == nil {
		t.Fatal("disks/snapshots wrong")
	}
	if len(f.secrets) != 1 || f.secrets["devbox-unrelated"] == nil {
		t.Fatal("secrets wrong: only this box's should go")
	}
	if !f.roles[config.SuspendRoleID].Deleted {
		t.Fatal("role not deleted")
	}
	if p, _ := e.b.PlanDestroy(e.ctx, true); !p.Empty() {
		t.Fatalf("second destroy not empty:\n%s", opsString(p))
	}

	// Coming back within 7 days undeletes the role rather than recreating it.
	f.addSecret("devbox-tailscale-oauth", "devbox", 1)
	f.addSecret("devbox-ssh-key", "devbox", 1)
	p = e.run(ModeUp)
	if !slices.Contains(actions(p), "update custom role "+config.SuspendRoleID) {
		t.Fatalf("expected undelete: %v", actions(p))
	}
	e.assertConverged()
}

func TestDestroyAllKeepsRoleUsedByOtherBox(t *testing.T) {
	e := setup(t)
	e.run(ModeUp)
	e.f.instances["devbox-test"] = &compute.Instance{Name: "devbox-test", Zone: "zones/europe-west4-a", Labels: map[string]string{"devbox": "devbox-test"}}
	p, err := e.b.PlanDestroy(e.ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(actions(p), "delete custom role "+config.SuspendRoleID) || len(p.Warnings) == 0 {
		t.Fatalf("role is shared and must be kept:\n%s", opsString(p))
	}
}

func TestPutSecretRotates(t *testing.T) {
	e := setup(t)
	rotated, err := e.b.PutSecret(e.ctx, "devbox-acme-api-KEY", "acme-api", []byte("v1"))
	if err != nil || rotated {
		t.Fatal(rotated, err)
	}
	rotated, err = e.b.PutSecret(e.ctx, "devbox-acme-api-KEY", "acme-api", []byte("v2"))
	if err != nil || !rotated {
		t.Fatal(rotated, err)
	}
	s := e.f.secrets["devbox-acme-api-KEY"]
	if s.s.Labels["devbox"] != "devbox" || len(s.versions) != 2 || s.versions[0].State != "DISABLED" || s.versions[1].State != "ENABLED" {
		t.Fatalf("versions: %+v %+v", s.versions[0], s.versions[1])
	}
	if err := e.b.DeleteSecret(e.ctx, "devbox-acme-api-KEY"); err != nil || e.f.secrets["devbox-acme-api-KEY"] != nil {
		t.Fatal("not deleted")
	}
}

func TestStoppedBoxStartsThenRestarts(t *testing.T) {
	e := setup(t)
	e.run(ModeUp)
	e.f.instances["devbox"].Status = "TERMINATED"
	if got := actions(e.run(ModeUp)); !slices.Equal(got, []string{"start instance devbox"}) {
		t.Fatalf("up on a stopped box did %v", got)
	}
	e.f.instances["devbox"].Status = "TERMINATED"
	e.b.C.MachineType = "e2-standard-4"
	e.run(ModeApply)
	if in := e.f.instances["devbox"]; in.Status != "RUNNING" || gcp.Last(in.MachineType) != "e2-standard-4" {
		t.Fatalf("%s %s", in.Status, in.MachineType)
	}
	e.assertConverged()
}
