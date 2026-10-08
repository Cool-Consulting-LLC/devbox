package box

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	crm "google.golang.org/api/cloudresourcemanager/v1"
	compute "google.golang.org/api/compute/v1"
	iam "google.golang.org/api/iam/v1"

	"github.com/Cool-Consulting-LLC/devbox/internal/config"
	"github.com/Cool-Consulting-LLC/devbox/internal/gcp"
)

var suspendPermissions = []string{"compute.instances.suspend"}

// Unconditional project roles for the Ops Agent.
var agentRoles = []string{"roles/logging.logWriter", "roles/monitoring.metricWriter"}

const (
	secretRole           = "roles/secretmanager.secretAccessor"
	secretConditionTitle = "devbox-secrets"
)

func (b *Box) saName() string {
	return fmt.Sprintf("projects/%s/serviceAccounts/%s", b.C.ProjectID, b.C.SAEmail())
}

func (b *Box) getSA(ctx context.Context) (*iam.ServiceAccount, error) {
	sa, err := b.G.IAM.Projects.ServiceAccounts.Get(b.saName()).Context(ctx).Do()
	if gcp.IsNotFound(err) {
		return nil, nil
	}
	return sa, err
}

func (b *Box) planServiceAccount(ctx context.Context, p *Plan) error {
	sa, err := b.getSA(ctx)
	if err != nil {
		return fmt.Errorf("service account: %w", err)
	}
	if sa != nil {
		if sa.Disabled {
			p.add(Op{Resource: "service account " + b.C.Name, Action: "update", Detail: "enable", Run: func(ctx context.Context) error {
				_, err := b.G.IAM.Projects.ServiceAccounts.Enable(b.saName(), &iam.EnableServiceAccountRequest{}).Context(ctx).Do()
				return err
			}})
		}
		return nil
	}
	p.add(Op{Resource: "service account " + b.C.Name, Action: "create", Detail: b.C.SAEmail(), Run: func(ctx context.Context) error {
		_, err := b.G.IAM.Projects.ServiceAccounts.Create("projects/"+b.C.ProjectID, &iam.CreateServiceAccountRequest{
			AccountId: b.C.Name,
			ServiceAccount: &iam.ServiceAccount{
				DisplayName: "devbox " + b.C.Name,
				Description: "Identity of the devbox instance " + b.C.Name + ". Managed by the devbox binary.",
			},
		}).Context(ctx).Do()
		if err != nil {
			return err
		}
		// Eventually consistent: wait until it is readable before anything binds it.
		return gcp.Retry(ctx, 60*time.Second, func(err error) bool { return err != nil }, func() error {
			sa, err := b.getSA(ctx)
			if err == nil && sa == nil {
				return fmt.Errorf("not visible yet")
			}
			return err
		})
	}})
	return nil
}

func (b *Box) planSuspendRole(ctx context.Context, p *Plan) error {
	name := b.C.SuspendRoleName()
	role, err := b.G.IAM.Projects.Roles.Get(name).Context(ctx).Do()
	switch {
	case gcp.IsNotFound(err):
		p.add(Op{Resource: "custom role " + config.SuspendRoleID, Action: "create", Detail: "compute.instances.suspend", Run: func(ctx context.Context) error {
			_, err := b.G.IAM.Projects.Roles.Create("projects/"+b.C.ProjectID, &iam.CreateRoleRequest{
				RoleId: config.SuspendRoleID,
				Role: &iam.Role{
					Title:               "devbox self-suspend",
					Description:         "Lets a devbox suspend itself when idle. Granted on the instance only.",
					IncludedPermissions: suspendPermissions,
					Stage:               "GA",
				},
			}).Context(ctx).Do()
			if gcp.Code(err) == 400 {
				return fmt.Errorf("%w\nthe role ID %s was deleted more than 7 days ago and cannot be reused for 37 days; see docs/03-teardown.md", err, config.SuspendRoleID)
			}
			return err
		}})
	case err != nil:
		return fmt.Errorf("custom role: %w", err)
	case role.Deleted:
		p.add(Op{Resource: "custom role " + config.SuspendRoleID, Action: "update", Detail: "undelete", Run: func(ctx context.Context) error {
			_, err := b.G.IAM.Projects.Roles.Undelete(name, &iam.UndeleteRoleRequest{Etag: role.Etag}).Context(ctx).Do()
			return err
		}})
	case !slices.Equal(role.IncludedPermissions, suspendPermissions):
		p.add(Op{Resource: "custom role " + config.SuspendRoleID, Action: "update", Detail: "permissions", Run: func(ctx context.Context) error {
			_, err := b.G.IAM.Projects.Roles.Patch(name, &iam.Role{IncludedPermissions: suspendPermissions}).
				UpdateMask("includedPermissions").Context(ctx).Do()
			return err
		}})
	}
	return nil
}

// secretCondition limits secretAccessor to devbox-* secrets. Secret Manager resource
// names in IAM conditions use the project number, not the ID.
func (b *Box) secretCondition(ctx context.Context) (*crm.Expr, error) {
	num, err := b.ProjectNumber(ctx)
	if err != nil {
		return nil, err
	}
	return &crm.Expr{
		Title:       secretConditionTitle,
		Description: "devbox: read secrets named " + config.SecretPrefix + "*",
		Expression:  fmt.Sprintf(`resource.name.startsWith("projects/%s/secrets/%s")`, num, config.SecretPrefix),
	}, nil
}

func (b *Box) getProjectPolicy(ctx context.Context) (*crm.Policy, error) {
	return b.G.CRM.Projects.GetIamPolicy(b.C.ProjectID, &crm.GetIamPolicyRequest{
		Options: &crm.GetPolicyOptions{RequestedPolicyVersion: 3},
	}).Context(ctx).Do()
}

// hasMember reports whether policy binds member to role with exactly cond (nil = none).
func hasMember(pol *crm.Policy, role, member string, cond *crm.Expr) bool {
	for _, bd := range pol.Bindings {
		if bd.Role != role || !sameExpr(bd.Condition, cond) {
			continue
		}
		if slices.Contains(bd.Members, member) {
			return true
		}
	}
	return false
}

func sameExpr(a, b *crm.Expr) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Expression == b.Expression && a.Title == b.Title
}

// missingProjectBindings lists the role names not yet granted.
func (b *Box) missingProjectBindings(pol *crm.Policy, cond *crm.Expr) []string {
	var miss []string
	for _, r := range agentRoles {
		if !hasMember(pol, r, b.C.SAMember(), nil) {
			miss = append(miss, r)
		}
	}
	if !hasMember(pol, secretRole, b.C.SAMember(), cond) {
		miss = append(miss, secretRole+" (devbox-* only)")
	}
	return miss
}

func (b *Box) planProjectIAM(ctx context.Context, p *Plan) error {
	pol, err := b.getProjectPolicy(ctx)
	if err != nil {
		return fmt.Errorf("project IAM policy: %w", err)
	}
	cond, err := b.secretCondition(ctx)
	if err != nil {
		return fmt.Errorf("project number: %w", err)
	}
	miss := b.missingProjectBindings(pol, cond)
	if len(miss) == 0 {
		return nil
	}
	p.add(Op{Resource: "project IAM", Action: "update", Detail: fmt.Sprintf("grant %v", miss), Run: func(ctx context.Context) error {
		return b.editProjectPolicy(ctx, func(pol *crm.Policy) {
			for _, r := range agentRoles {
				addMember(pol, r, b.C.SAMember(), nil)
			}
			addMember(pol, secretRole, b.C.SAMember(), cond)
		})
	}})
	return nil
}

func addMember(pol *crm.Policy, role, member string, cond *crm.Expr) {
	if hasMember(pol, role, member, cond) {
		return
	}
	for _, bd := range pol.Bindings {
		if bd.Role == role && sameExpr(bd.Condition, cond) {
			bd.Members = append(bd.Members, member)
			return
		}
	}
	pol.Bindings = append(pol.Bindings, &crm.Binding{Role: role, Members: []string{member}, Condition: cond})
}

// removeMember drops member (and its deleted: form) from every binding of the roles.
func removeMember(pol *crm.Policy, member string, roles ...string) bool {
	changed := false
	var kept []*crm.Binding
	for _, bd := range pol.Bindings {
		if slices.Contains(roles, bd.Role) {
			n := len(bd.Members)
			bd.Members = slices.DeleteFunc(bd.Members, func(m string) bool {
				return m == member || strings.HasPrefix(m, "deleted:"+member+"?")
			})
			changed = changed || n != len(bd.Members)
			if len(bd.Members) == 0 {
				continue
			}
		}
		kept = append(kept, bd)
	}
	pol.Bindings = kept
	return changed
}

// editProjectPolicy is a read-modify-write that retries on etag races and on a
// service account IAM cannot see yet.
func (b *Box) editProjectPolicy(ctx context.Context, edit func(*crm.Policy)) error {
	retryable := func(err error) bool { c := gcp.Code(err); return c == 409 || c == 400 || c == 412 }
	return gcp.Retry(ctx, 90*time.Second, retryable, func() error {
		pol, err := b.getProjectPolicy(ctx)
		if err != nil {
			return err
		}
		edit(pol)
		pol.Version = 3
		_, err = b.G.CRM.Projects.SetIamPolicy(b.C.ProjectID, &crm.SetIamPolicyRequest{Policy: pol}).Context(ctx).Do()
		return err
	})
}

// --- instance-level grant of the self-suspend role ---

func (b *Box) planInstanceIAM(ctx context.Context, p *Plan) error {
	pol, err := b.G.Compute.Instances.GetIamPolicy(b.C.ProjectID, b.C.Zone, b.C.Name).OptionsRequestedPolicyVersion(3).Context(ctx).Do()
	if err != nil && !gcp.IsNotFound(err) {
		return fmt.Errorf("instance IAM policy: %w", err)
	}
	// A grant on an instance that is about to be replaced dies with it.
	if err == nil && !b.instanceWillChange(p) {
		for _, bd := range pol.Bindings {
			if bd.Role == b.C.SuspendRoleName() && slices.Contains(bd.Members, b.C.SAMember()) {
				return nil
			}
		}
	}
	p.add(Op{Resource: "instance IAM", Action: "update", Detail: "grant " + config.SuspendRoleID + " to its own service account", Run: b.grantSelfSuspend})
	return nil
}

func (b *Box) grantSelfSuspend(ctx context.Context) error {
	retryable := func(err error) bool { c := gcp.Code(err); return c == 409 || c == 400 || c == 412 }
	return gcp.Retry(ctx, 90*time.Second, retryable, func() error {
		pol, err := b.G.Compute.Instances.GetIamPolicy(b.C.ProjectID, b.C.Zone, b.C.Name).OptionsRequestedPolicyVersion(3).Context(ctx).Do()
		if err != nil {
			return err
		}
		found := false
		for _, bd := range pol.Bindings {
			if bd.Role == b.C.SuspendRoleName() {
				if slices.Contains(bd.Members, b.C.SAMember()) {
					return nil
				}
				bd.Members = append(bd.Members, b.C.SAMember())
				found = true
			}
		}
		if !found {
			pol.Bindings = append(pol.Bindings, &compute.Binding{Role: b.C.SuspendRoleName(), Members: []string{b.C.SAMember()}})
		}
		_, err = b.G.Compute.Instances.SetIamPolicy(b.C.ProjectID, b.C.Zone, b.C.Name, &compute.ZoneSetPolicyRequest{Policy: pol}).Context(ctx).Do()
		return err
	})
}
