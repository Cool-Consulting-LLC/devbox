package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	crm "google.golang.org/api/cloudresourcemanager/v1"
	compute "google.golang.org/api/compute/v1"
	serviceusage "google.golang.org/api/serviceusage/v1"

	"github.com/Cool-Consulting-LLC/devbox/internal/gcp"
)

// RequiredAPIs: the plan's five plus the three the binary itself calls.
var RequiredAPIs = []string{
	"compute.googleapis.com",
	"secretmanager.googleapis.com",
	"iap.googleapis.com",
	"monitoring.googleapis.com",
	"oslogin.googleapis.com",
	"iam.googleapis.com",
	"cloudresourcemanager.googleapis.com",
	"logging.googleapis.com",
}

// Permissions `up` exercises. Owner has all of them; editor lacks the IAM ones.
var neededPermissions = []string{
	"compute.instances.create",
	"compute.firewalls.create",
	"compute.resourcePolicies.create",
	"iam.serviceAccounts.create",
	"iam.serviceAccounts.actAs",
	"iam.roles.create",
	"resourcemanager.projects.setIamPolicy",
	"secretmanager.secrets.create",
	"monitoring.alertPolicies.create",
	"serviceusage.services.enable",
}

const osAdminPermission = "compute.instances.osAdminLogin"

const (
	stOK      = "ok"
	stFail    = "FAIL"
	stWarn    = "warn"
	stMissing = "MISSING"
	stFixed   = "fixed"
)

type check struct{ name, value, status, hint string }

// PreflightResult carries what later commands need from preflight.
type PreflightResult struct {
	OK        bool
	Email     string
	PosixUser string
}

// Preflight is a read-only survey. With fix, it enables missing APIs and nothing else.
func (b *Box) Preflight(ctx context.Context, fix bool, w io.Writer) (*PreflightResult, error) {
	res := &PreflightResult{OK: true}
	var checks []check
	add := func(c check) {
		checks = append(checks, c)
		if c.status == stFail || c.status == stMissing {
			res.OK = false
		}
	}
	defer func() { printChecks(w, checks) }()

	proj, err := b.G.CRM.Projects.Get(b.C.ProjectID).Context(ctx).Do()
	if err != nil {
		add(check{"project", b.C.ProjectID, stFail, fmt.Sprintf("cannot read it: %v", short(err))})
		return res, nil
	}
	if proj.LifecycleState != "ACTIVE" {
		add(check{"project", b.C.ProjectID, stFail, "state is " + proj.LifecycleState})
		return res, nil
	}
	add(check{"project", b.C.ProjectID, stOK, ""})

	email, err := b.G.CallerEmail(ctx)
	if err != nil {
		add(check{"identity", "", stFail, err.Error()})
		return res, nil
	}
	res.Email = email

	// Permissions: test the real thing, and show the roles for orientation.
	tp, err := b.G.CRM.Projects.TestIamPermissions(b.C.ProjectID, &crm.TestIamPermissionsRequest{
		Permissions: append(slices.Clone(neededPermissions), osAdminPermission),
	}).Context(ctx).Do()
	if err != nil {
		add(check{"permissions", email, stFail, short(err)})
		return res, nil
	}
	var missing []string
	for _, p := range neededPermissions {
		if !slices.Contains(tp.Permissions, p) {
			missing = append(missing, p)
		}
	}
	roles := b.callerRoles(ctx, email)
	if len(missing) > 0 {
		add(check{"permissions", email + " " + roles, stFail, "missing " + strings.Join(missing, ", ") + ". roles/owner covers all; editor also needs roles/resourcemanager.projectIamAdmin and roles/iam.roleAdmin"})
	} else {
		add(check{"permissions", email + " " + roles, stOK, ""})
	}

	b.checkAPIs(ctx, fix, add)

	// OS Login: the POSIX name is what the Tailscale SSH policy needs.
	if lp, err := b.G.OSLogin.Users.GetLoginProfile("users/" + email).ProjectId(b.C.ProjectID).Context(ctx).Do(); err != nil {
		add(check{"os login", "", stFail, short(err)})
	} else {
		name := ""
		for _, pa := range lp.PosixAccounts {
			if pa.Primary || name == "" {
				name = pa.Username
			}
		}
		if name == "" {
			add(check{"os login", "", stWarn, "no POSIX account yet; it is created on first login. Expect a name derived from " + email})
		} else {
			res.PosixUser = name
			add(check{"os login", "your posix name: " + name, stOK, ""})
		}
	}
	if slices.Contains(tp.Permissions, osAdminPermission) {
		add(check{"os admin login", "roles/compute.osAdminLogin", stOK, ""})
	} else {
		add(check{"os admin login", "", stFail, "grant yourself roles/compute.osAdminLogin: only admins get /srv/code and the machine key"})
	}

	b.checkNetwork(ctx, add)
	b.checkExposure(ctx, add)
	b.checkOrgPolicies(ctx, add)
	b.checkMachine(ctx, add)

	for _, s := range []struct{ id, hint string }{
		{b.C.Tailscale.OAuthSecret, "create it: README.md, How to set it up, step 3"},
		{b.C.SSHKey.Secret, "create it: README.md, How to set it up, step 4"},
	} {
		ok, err := b.SecretExists(ctx, s.id)
		switch {
		case err != nil:
			add(check{"secret", s.id, stFail, short(err)})
		case !ok:
			add(check{"secret", s.id, stMissing, s.hint})
		default:
			add(check{"secret", s.id, stOK, ""})
		}
	}
	return res, nil
}

func (b *Box) callerRoles(ctx context.Context, email string) string {
	pol, err := b.getProjectPolicy(ctx)
	if err != nil {
		return ""
	}
	var rs []string
	for _, bd := range pol.Bindings {
		for _, m := range bd.Members {
			if m == "user:"+email || m == "serviceAccount:"+email {
				rs = append(rs, bd.Role)
			}
		}
	}
	for _, r := range []string{"roles/owner", "roles/editor"} {
		if slices.Contains(rs, r) {
			return r
		}
	}
	if len(rs) == 0 {
		return "(roles via groups)"
	}
	return strings.Join(rs, ",")
}

func (b *Box) checkAPIs(ctx context.Context, fix bool, add func(check)) {
	enabled := map[string]bool{}
	err := b.G.Usage.Services.List("projects/"+b.C.ProjectID).Filter("state:ENABLED").Pages(ctx, func(r *serviceusage.ListServicesResponse) error {
		for _, s := range r.Services {
			enabled[s.Config.Name] = true
		}
		return nil
	})
	if err != nil {
		add(check{"apis", "", stFail, "cannot list services (is serviceusage.googleapis.com enabled?): " + short(err)})
		return
	}
	var missing []string
	for _, a := range RequiredAPIs {
		if !enabled[a] {
			missing = append(missing, a)
		}
	}
	names := strings.ReplaceAll(strings.Join(RequiredAPIs, ", "), ".googleapis.com", "")
	if len(missing) == 0 {
		add(check{"apis", names, stOK, ""})
		return
	}
	if !fix {
		add(check{"apis", strings.Join(missing, ", "), stMissing, "run `devbox preflight --fix` to enable them"})
		return
	}
	op, err := b.G.Usage.Services.BatchEnable("projects/"+b.C.ProjectID, &serviceusage.BatchEnableServicesRequest{ServiceIds: missing}).Context(ctx).Do()
	for err == nil && !op.Done {
		time.Sleep(3 * time.Second)
		op, err = b.G.Usage.Operations.Get(op.Name).Context(ctx).Do()
	}
	if err == nil && op.Error != nil {
		err = fmt.Errorf("%s", op.Error.Message)
	}
	if err != nil {
		add(check{"apis", strings.Join(missing, ", "), stFail, "enable failed: " + short(err)})
		return
	}
	add(check{"apis", "enabled " + strings.Join(missing, ", "), stFixed, ""})
}

func (b *Box) checkNetwork(ctx context.Context, add func(check)) {
	if _, err := b.G.Compute.Zones.Get(b.C.ProjectID, b.C.Zone).Context(ctx).Do(); err != nil {
		add(check{"zone/region", b.C.Zone, stFail, short(err)})
		return
	}
	add(check{"zone/region", b.C.Zone + " / " + b.C.Region(), stOK, ""})

	if _, err := b.G.Compute.Networks.Get(b.C.ProjectID, b.C.Network.Name).Context(ctx).Do(); err != nil {
		add(check{"network", b.C.Network.Name, stFail, short(err)})
		return
	}
	sub, err := b.G.Compute.Subnetworks.Get(b.C.ProjectID, b.C.Region(), b.C.Network.Subnet).Context(ctx).Do()
	if err != nil {
		add(check{"network", b.C.Network.Name + " / " + b.C.Network.Subnet, stFail,
			fmt.Sprintf("no subnet %q in %s: the zone must be inside the subnet's region (%s)", b.C.Network.Subnet, b.C.Region(), short(err))})
		return
	}
	if gcp.Last(sub.Network) != b.C.Network.Name {
		add(check{"network", b.C.Network.Name + " / " + b.C.Network.Subnet, stFail, "subnet belongs to network " + gcp.Last(sub.Network)})
		return
	}
	add(check{"network", b.C.Network.Name + " / " + b.C.Network.Subnet + " " + sub.IpCidrRange, stOK, ""})

	if b.C.WantExternalIP() {
		add(check{"egress", "external IP", stOK, ""})
		return
	}
	if !sub.PrivateIpGoogleAccess {
		add(check{"egress", "no external IP", stFail, "Private Google Access is off on the subnet; the box would reach nothing"})
		return
	}
	routers, err := b.G.Compute.Routers.List(b.C.ProjectID, b.C.Region()).Context(ctx).Do()
	nat := ""
	if err == nil {
		for _, r := range routers.Items {
			if gcp.Last(r.Network) == b.C.Network.Name && len(r.Nats) > 0 {
				nat = r.Name
			}
		}
	}
	if nat == "" {
		add(check{"egress", "no external IP", stWarn, "no Cloud NAT router found for this network in " + b.C.Region() + ". Does the subnet have internet egress? apt and Tailscale need it"})
	} else {
		add(check{"egress", "no external IP, NAT on router " + nat, stWarn, "a NAT exists; confirm it covers subnet " + b.C.Network.Subnet + ", which no API can answer for every config"})
	}
}

// checkExposure looks for firewall rules this binary did not create that let
// traffic reach the box. GCP's network called default ships default-allow-ssh
// from 0.0.0.0/0, which with an external IP puts sshd on the internet.
func (b *Box) checkExposure(ctx context.Context, add func(check)) {
	internet, internal, err := b.exposure(ctx)
	if err != nil {
		add(check{"exposure", "", stWarn, "cannot list firewall rules: " + short(err)})
		return
	}
	switch {
	case len(internet) > 0 && b.C.WantExternalIP():
		add(check{"exposure", strings.Join(internet, ", "), stFail, exposureHint})
	case len(internet) > 0:
		add(check{"exposure", strings.Join(internet, ", "), stWarn, "these rules allow traffic from the internet; harmless without an external IP, but they apply the moment one is added"})
	}
	if len(internal) > 0 {
		add(check{"exposure", strings.Join(internal, ", "), stWarn, "other machines in network " + b.C.Network.Name + " can reach the box through these rules"})
	}
	if len(internet) == 0 && len(internal) == 0 {
		add(check{"exposure", "only devbox's own rules", stOK, ""})
	}
}

const exposureHint = "these rules open the box to the internet. Delete them, scope them with target tags, or use a dedicated network (README.md, step 2)"

func (b *Box) exposure(ctx context.Context) (internet, internal []string, err error) {
	var rules []*compute.Firewall
	err = b.G.Compute.Firewalls.List(b.C.ProjectID).Pages(ctx, func(l *compute.FirewallList) error {
		rules = append(rules, l.Items...)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	own := b.desiredFirewalls()
	internet, internal = exposingRules(rules, b.C.Network.Name, b.C.SAEmail(), func(n string) bool { return own[n] != nil })
	return internet, internal, nil
}

// ErrExposed marks a box that would be reachable from the internet.
var ErrExposed = errors.New("the box would be reachable from the internet")

// CheckExposure is preflight's exposure check on its own, for up and apply: an
// error if the box has, or will get, an external IP and a firewall rule devbox
// did not create admits the internet. A rule list it cannot read is an error
// too, since then nothing says the box is safe.
func (b *Box) CheckExposure(ctx context.Context) error {
	if !b.C.WantExternalIP() {
		return nil
	}
	internet, _, err := b.exposure(ctx)
	if err != nil {
		return fmt.Errorf("%w? cannot list firewall rules to tell: %s", ErrExposed, short(err))
	}
	if len(internet) > 0 {
		return fmt.Errorf("%w:\n  %s\n%s", ErrExposed, strings.Join(internet, "\n  "), exposureHint)
	}
	return nil
}

// exposingRules names the enabled ingress allow rules on network that apply to
// a VM with service account sa and no network tags, split by whether any
// source is a public address range. own reports the binary's own rules.
func exposingRules(rules []*compute.Firewall, network, sa string, own func(string) bool) (internet, internal []string) {
	for _, r := range rules {
		if own(r.Name) || r.Disabled || r.Direction != "INGRESS" || len(r.Allowed) == 0 || gcp.Last(r.Network) != network {
			continue
		}
		// The box carries no network tags, so a tag-targeted rule never reaches it.
		if len(r.TargetTags) > 0 || (len(r.TargetServiceAccounts) > 0 && !slices.Contains(r.TargetServiceAccounts, sa)) {
			continue
		}
		public := false
		for _, src := range r.SourceRanges {
			if p, err := netip.ParsePrefix(src); err == nil && src != iapRange && !p.Addr().IsPrivate() && !p.Addr().IsLoopback() {
				public = true
			}
		}
		desc := r.Name + " (" + allowedString(r.Allowed) + " from " + strings.Join(append(slices.Clone(r.SourceRanges), r.SourceTags...), ",") + ")"
		if public {
			internet = append(internet, desc)
		} else {
			internal = append(internal, desc)
		}
	}
	return internet, internal
}

func (b *Box) checkOrgPolicies(ctx context.Context, add func(check)) {
	get := func(c string) *crm.OrgPolicy {
		p, err := b.G.CRM.Projects.GetEffectiveOrgPolicy("projects/"+b.C.ProjectID, &crm.GetEffectiveOrgPolicyRequest{Constraint: "constraints/" + c}).Context(ctx).Do()
		if err != nil {
			return nil
		}
		return p
	}
	var notes []string
	failed := false
	if p := get("compute.vmExternalIpAccess"); p != nil && p.ListPolicy != nil {
		lp := p.ListPolicy
		if lp.AllValues == "DENY" || len(lp.AllowedValues) > 0 {
			notes = append(notes, "vmExternalIpAccess restricts external IPs")
			if b.C.WantExternalIP() {
				failed = true
				add(check{"org policies", "compute.vmExternalIpAccess", stFail, "set external_ip: false and make sure the subnet has Cloud NAT"})
			}
		}
	}
	if p := get("iam.disableServiceAccountCreation"); p != nil && p.BooleanPolicy != nil && p.BooleanPolicy.Enforced {
		failed = true
		add(check{"org policies", "iam.disableServiceAccountCreation", stFail, "the box needs its own service account; ask an org admin for an exception on this project"})
	}
	if p := get("compute.requireShieldedVm"); p != nil && p.BooleanPolicy != nil && p.BooleanPolicy.Enforced {
		notes = append(notes, "requireShieldedVm (satisfied)")
	}
	if p := get("compute.requireOsLogin"); p != nil && p.BooleanPolicy != nil && p.BooleanPolicy.Enforced {
		notes = append(notes, "requireOsLogin (satisfied)")
	}
	if failed {
		return
	}
	if len(notes) == 0 {
		add(check{"org policies", "none enforced", stOK, ""})
	} else {
		add(check{"org policies", strings.Join(notes, "; "), stOK, ""})
	}
}

func (b *Box) checkMachine(ctx context.Context, add func(check)) {
	mt, err := b.G.Compute.MachineTypes.Get(b.C.ProjectID, b.C.Zone, b.C.MachineType).Context(ctx).Do()
	if err != nil {
		add(check{"suspend support", b.C.MachineType, stFail, short(err)})
		return
	}
	gib := float64(mt.MemoryMb) / 1024
	switch {
	case mt.MemoryMb > 208*1024:
		add(check{"suspend support", fmt.Sprintf("%s, %.0f GiB", mt.Name, gib), stFail, "suspend is limited to 208 GB of memory"})
	case len(mt.Accelerators) > 0:
		add(check{"suspend support", mt.Name, stFail, "machine types with GPUs cannot suspend"})
	case !strings.HasPrefix(mt.Name, "e2-"):
		add(check{"suspend support", fmt.Sprintf("%s, %.0f GiB", mt.Name, gib), stWarn, "not E2: guest memory metrics need an agent, and check this family supports suspend"})
	default:
		add(check{"suspend support", fmt.Sprintf("%s, %.0f GiB", mt.Name, gib), stOK, ""})
	}

	// pd-balanced counts against SSD_TOTAL_GB; a suspended memory image against DISKS_TOTAL_GB.
	region, err := b.G.Compute.Regions.Get(b.C.ProjectID, b.C.Region()).Context(ctx).Do()
	if err != nil {
		return
	}
	for _, q := range region.Quotas {
		need := 0.0
		switch q.Metric {
		case "SSD_TOTAL_GB":
			need = float64(b.C.BootDiskGB + b.C.CodeDiskGB)
		case "DISKS_TOTAL_GB":
			need = gib
		default:
			continue
		}
		if q.Limit-q.Usage < need {
			add(check{"quota", fmt.Sprintf("%s %.0f/%.0f", q.Metric, q.Usage, q.Limit), stWarn, fmt.Sprintf("about %.0f GB more may be needed", need)})
		}
	}
}

func printChecks(w io.Writer, checks []check) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range checks {
		fmt.Fprintf(tw, "  %s\t%s\t%s", c.name, c.value, c.status)
		if c.hint != "" {
			fmt.Fprintf(tw, "\t-> %s", c.hint)
		}
		fmt.Fprintln(tw)
	}
	tw.Flush()
}

// short trims a Google API error to its message.
func short(err error) string {
	s := err.Error()
	if i := strings.Index(s, "googleapi: "); i >= 0 {
		s = s[i+len("googleapi: "):]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// PosixUser is the caller's OS Login username, the login name on the box.
func (b *Box) PosixUser(ctx context.Context) (string, error) {
	email, err := b.G.CallerEmail(ctx)
	if err != nil {
		return "", err
	}
	lp, err := b.G.OSLogin.Users.GetLoginProfile("users/" + email).ProjectId(b.C.ProjectID).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	for _, pa := range lp.PosixAccounts {
		if pa.Primary {
			return pa.Username, nil
		}
	}
	if len(lp.PosixAccounts) > 0 {
		return lp.PosixAccounts[0].Username, nil
	}
	return "", fmt.Errorf("no POSIX account for %s yet; log in once with `gcloud compute ssh --tunnel-through-iap`", email)
}
