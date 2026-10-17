package box

import (
	"strings"
	"testing"
	"time"

	crm "google.golang.org/api/cloudresourcemanager/v1"
	compute "google.golang.org/api/compute/v1"

	"github.com/Cool-Consulting-LLC/devbox/internal/config"
)

func testBox(t *testing.T) *Box {
	c := config.Defaults()
	c.ProjectID = "my-devbox-project"
	c.Network = config.Network{Name: "devbox", Subnet: "devbox-europe-west4"}
	c.Git.Name, c.Git.Email = "A", "a@example.com"
	c.Alerts.Email = "a@example.com"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return New(c, nil, nil)
}

func TestProjectSecretID(t *testing.T) {
	id, err := ProjectSecretID("devbox", "acme-api", "OPENAI_API_KEY")
	if err != nil || id != "devbox-acme-api-OPENAI_API_KEY" {
		t.Fatalf("%s %v", id, err)
	}
	if id, _ := ProjectSecretID("devbox-test", "acme-api", "K"); id != "devbox-test-acme-api-K" {
		t.Fatalf("box name must lead: %s", id)
	}
	if id, _ := ProjectSecretID("devbox", "a.b", "K"); id != "devbox-a_b-K" {
		t.Fatalf("dots: %s", id)
	}
	for _, bad := range [][2]string{{"../x", "K"}, {"p", "1K"}, {"p", "K-Y"}} {
		if _, err := ProjectSecretID("devbox", bad[0], bad[1]); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestPolicyMembers(t *testing.T) {
	b := testBox(t)
	cond := &crm.Expr{Title: "devbox-secrets", Expression: `resource.name.startsWith("projects/1/secrets/devbox-")`}
	pol := &crm.Policy{Bindings: []*crm.Binding{{Role: "roles/owner", Members: []string{"user:me@example.com"}}}}
	if got := b.missingProjectBindings(pol); len(got) != 2 {
		t.Fatalf("missing = %v", got)
	}
	for _, r := range agentRoles {
		addMember(pol, r, b.C.SAMember(), nil)
		addMember(pol, r, b.C.SAMember(), nil) // idempotent
	}
	if got := b.missingProjectBindings(pol); len(got) != 0 {
		t.Fatalf("still missing %v", got)
	}
	if len(pol.Bindings) != 3 {
		t.Fatalf("bindings = %d", len(pol.Bindings))
	}
	// A project-wide secretAccessor, conditioned or not, is the legacy grant to revoke.
	if b.hasProjectSecretGrant(pol) {
		t.Fatal("no secret grant yet")
	}
	addMember(pol, secretRole, b.C.SAMember(), cond)
	if !b.hasProjectSecretGrant(pol) {
		t.Fatal("conditional grant not found")
	}
	if hasMember(pol, secretRole, b.C.SAMember(), nil) || !hasMember(pol, secretRole, b.C.SAMember(), cond) {
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

func TestExposingRules(t *testing.T) {
	sa := "devbox@p.iam.gserviceaccount.com"
	net := "projects/p/global/networks/default"
	tcp22 := []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"22"}}}
	rules := []*compute.Firewall{
		{Name: "default-allow-ssh", Network: net, Direction: "INGRESS", SourceRanges: []string{"0.0.0.0/0"}, Allowed: tcp22},
		{Name: "default-allow-internal", Network: net, Direction: "INGRESS", SourceRanges: []string{"10.128.0.0/9"}, Allowed: tcp22},
		{Name: "web", Network: net, Direction: "INGRESS", SourceRanges: []string{"0.0.0.0/0"}, Allowed: tcp22, TargetTags: []string{"web"}},
		{Name: "other-sa", Network: net, Direction: "INGRESS", SourceRanges: []string{"0.0.0.0/0"}, Allowed: tcp22, TargetServiceAccounts: []string{"x@p.iam.gserviceaccount.com"}},
		{Name: "to-box", Network: net, Direction: "INGRESS", SourceRanges: []string{"198.51.100.0/24"}, Allowed: tcp22, TargetServiceAccounts: []string{sa}},
		{Name: "off", Network: net, Direction: "INGRESS", SourceRanges: []string{"0.0.0.0/0"}, Allowed: tcp22, Disabled: true},
		{Name: "deny", Network: net, Direction: "INGRESS", SourceRanges: []string{"0.0.0.0/0"}, Denied: []*compute.FirewallDenied{{IPProtocol: "all"}}},
		{Name: "elsewhere", Network: "projects/p/global/networks/other", Direction: "INGRESS", SourceRanges: []string{"0.0.0.0/0"}, Allowed: tcp22},
		{Name: "devbox-allow-iap-ssh", Network: net, Direction: "INGRESS", SourceRanges: []string{iapRange}, Allowed: tcp22},
	}
	own := func(n string) bool { return n == "devbox-allow-iap-ssh" }
	internet, internal := exposingRules(rules, "default", sa, own)
	if len(internet) != 2 || !strings.HasPrefix(internet[0], "default-allow-ssh ") || !strings.HasPrefix(internet[1], "to-box ") {
		t.Fatalf("internet: %v", internet)
	}
	if len(internal) != 1 || !strings.HasPrefix(internal[0], "default-allow-internal ") {
		t.Fatalf("internal: %v", internal)
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
	if specs[0].threshold != 0.95 || specs[1].threshold != 0.05 || specs[2].threshold != 85 {
		t.Fatalf("thresholds: CPU is a 0-1 fraction, disk a percentage: %v %v %v", specs[0].threshold, specs[1].threshold, specs[2].threshold)
	}
}

func TestE2StandardVCPUs(t *testing.T) {
	for mt, want := range map[string]int{"e2-standard-2": 2, "e2-standard-4": 4, "e2-standard-32": 32} {
		if n, ok := e2StandardVCPUs(mt); !ok || n != want {
			t.Errorf("%s: %d %v", mt, n, ok)
		}
	}
	for _, mt := range []string{"e2-highmem-4", "n2-standard-4", "e2-standard-4x", "e2-standard-"} {
		if _, ok := e2StandardVCPUs(mt); ok {
			t.Errorf("%s accepted", mt)
		}
	}
}

func TestPrintCost(t *testing.T) {
	b := testBox(t) // e2-standard-4 in europe-west4, external IP
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	var out strings.Builder
	b.printCost(&out, costInputs{bootGB: 30, codeGB: 50, now: now, runHours: 73})
	// disks 9.60 + compute 107.70 + IP 3.65 running; 9.60 + 2.72 suspended.
	// 73 h running and 167 h suspended so far, then 744/240 of that for October.
	for _, want := range []string{"$14.91", "73.0 h running", "$46.23", "$12.32", "$120.95", "e2-standard-4 $107.70"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}

	out.Reset()
	b.C.Zone = "us-east1-b"
	b.printCost(&out, costInputs{bootGB: 30, codeGB: 50, now: now, runHours: 73})
	if !strings.Contains(out.String(), "no compute price for e2-standard-4 in us-east1") {
		t.Errorf("unknown region not flagged:\n%s", out.String())
	}
}
