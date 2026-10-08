package box

import (
	"strings"
	"testing"

	crm "google.golang.org/api/cloudresourcemanager/v1"

	"github.com/Cool-Consulting-LLC/devbox/internal/config"
)

func testBox(t *testing.T) *Box {
	c := config.Defaults()
	c.ProjectID = "my-devbox-project"
	c.Git.Name, c.Git.Email = "A", "a@example.com"
	c.Alerts.Email = "a@example.com"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return New(c, nil, nil)
}

func TestProjectSecretID(t *testing.T) {
	id, err := ProjectSecretID("acme-api", "OPENAI_API_KEY")
	if err != nil || id != "devbox-acme-api-OPENAI_API_KEY" {
		t.Fatalf("%s %v", id, err)
	}
	if id, _ := ProjectSecretID("a.b", "K"); id != "devbox-a_b-K" {
		t.Fatalf("dots: %s", id)
	}
	for _, bad := range [][2]string{{"../x", "K"}, {"p", "1K"}, {"p", "K-Y"}} {
		if _, err := ProjectSecretID(bad[0], bad[1]); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestPolicyMembers(t *testing.T) {
	b := testBox(t)
	cond := &crm.Expr{Title: secretConditionTitle, Expression: `resource.name.startsWith("projects/1/secrets/devbox-")`}
	pol := &crm.Policy{Bindings: []*crm.Binding{{Role: "roles/owner", Members: []string{"user:me@example.com"}}}}
	if got := b.missingProjectBindings(pol, cond); len(got) != 3 {
		t.Fatalf("missing = %v", got)
	}
	for _, r := range agentRoles {
		addMember(pol, r, b.C.SAMember(), nil)
	}
	addMember(pol, secretRole, b.C.SAMember(), cond)
	addMember(pol, secretRole, b.C.SAMember(), cond) // idempotent
	if got := b.missingProjectBindings(pol, cond); len(got) != 0 {
		t.Fatalf("still missing %v", got)
	}
	if len(pol.Bindings) != 4 {
		t.Fatalf("bindings = %d", len(pol.Bindings))
	}
	// An unconditional secretAccessor does not satisfy the conditional one.
	pol2 := &crm.Policy{Bindings: []*crm.Binding{{Role: secretRole, Members: []string{b.C.SAMember()}}}}
	if !hasMember(pol2, secretRole, b.C.SAMember(), nil) || hasMember(pol2, secretRole, b.C.SAMember(), cond) {
		t.Fatal("condition matching wrong")
	}

	pol.Bindings[0].Members = append(pol.Bindings[0].Members, "deleted:"+b.C.SAMember()+"?uid=123")
	pol.Bindings = append(pol.Bindings, &crm.Binding{Role: "roles/logging.logWriter", Members: []string{"deleted:" + b.C.SAMember() + "?uid=9"}})
	if !removeMember(pol, b.C.SAMember(), append([]string{secretRole}, agentRoles...)...) {
		t.Fatal("nothing removed")
	}
	if len(pol.Bindings) != 1 || pol.Bindings[0].Role != "roles/owner" {
		t.Fatalf("left %+v", pol.Bindings)
	}
}

func TestFirewalls(t *testing.T) {
	b := testBox(t)
	fw := b.desiredFirewalls()
	if len(fw) != 1 {
		t.Fatalf("break glass present while disabled")
	}
	iap := fw[b.C.FirewallIAPName()]
	if iap.SourceRanges[0] != iapRange || iap.TargetServiceAccounts[0] != b.C.SAEmail() || len(iap.TargetTags) != 0 {
		t.Fatalf("iap rule wrong: %+v", iap)
	}
	b.C.Glass = config.BreakGlass{Enabled: true, SourceCIDR: "203.0.113.7/32", AllowMosh: true}
	fw = b.desiredFirewalls()
	g := fw[b.C.FirewallGlassName()]
	if g == nil || allowedString(g.Allowed) != "tcp:22 udp:60000-61000" {
		t.Fatalf("glass rule wrong: %+v", g)
	}
	if fwDiff(iap, iap) != "" {
		t.Fatal("identical rules differ")
	}
}

func TestDesiredInstance(t *testing.T) {
	b := testBox(t)
	in := b.desiredInstance()
	md := metadataMap(in.Metadata)
	if md["enable-oslogin"] != "TRUE" || md["ssh-keys"] != "" || !strings.HasPrefix(md["user-data"], "#cloud-config") {
		t.Fatal("metadata wrong")
	}
	if !hasScope(in.ServiceAccounts[0].Scopes) {
		t.Fatal("cloud-platform scope missing")
	}
	if in.Disks[1].DeviceName != "code" || in.Disks[1].AutoDelete {
		t.Fatal("code disk must be device 'code' and never auto-deleted")
	}
	if s := in.ShieldedInstanceConfig; !s.EnableSecureBoot || !s.EnableVtpm || !s.EnableIntegrityMonitoring {
		t.Fatal("shielded config")
	}
	b.C.ExternalIP = new(bool)
	if len(b.desiredInstance().NetworkInterfaces[0].AccessConfigs) != 0 {
		t.Fatal("external IP present when disabled")
	}
}

func TestAlertSpecs(t *testing.T) {
	b := testBox(t)
	specs := b.alertSpecs(42)
	if len(specs) != 3 {
		t.Fatal(len(specs))
	}
	for _, s := range specs {
		if !strings.Contains(s.filter, `resource.labels.instance_id = "42"`) {
			t.Errorf("%s not pinned to the instance", s.key)
		}
		p := b.desiredPolicy(s, "projects/x/notificationChannels/1")
		if p.Conditions[0].ConditionThreshold.EvaluationMissingData != "EVALUATION_MISSING_DATA_INACTIVE" {
			t.Errorf("%s: missing data must be INACTIVE", s.key)
		}
		if policyDiff(p, b.desiredPolicy(s, "projects/x/notificationChannels/1")) {
			t.Errorf("%s: identical policies differ", s.key)
		}
	}
	if specs[0].threshold != 0.8 || specs[1].threshold != 0.05 || specs[2].threshold != 85 {
		t.Fatalf("thresholds: CPU is a 0-1 fraction, disk a percentage: %v %v %v", specs[0].threshold, specs[1].threshold, specs[2].threshold)
	}
}
