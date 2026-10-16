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
	secretmanager "google.golang.org/api/secretmanager/v1"

	"github.com/Cool-Consulting-LLC/devbox/internal/config"
	"github.com/Cool-Consulting-LLC/devbox/internal/gcp"
)

var suspendPermissions = []string{"compute.instances.suspend"}

// Unconditional project roles for the Ops Agent.
var agentRoles = []string{"roles/logging.logWriter", "roles/monitoring.metricWriter"}

const secretRole = "roles/secretmanager.secretAccessor"

func (b *Box) saName() string {
	return fmt.Sprintf("projects/%s/serviceAccounts/%s", b.C.ProjectID, b.C.SAEmail())
}

func (b *Box) getSA(ctx context.Context) (*iam.ServiceAccount, error) {
	sa, err := b.G.IAM.Projects.ServiceAccounts.Get(b.saName()).Context(ctx).Do()
	if gcp.IsNotFound(err) {
		return nil, nil
	}
	if err == nil && sa.Description != b.saDescription() {
		return sa, b.notOurs("service account", b.C.SAEmail(), "delete it if it is a leftover")
	}
	return sa, err
}

func (b *Box) saDescription() string {
	return "Identity of the devbox instance " + b.C.Name + ". Managed by the devbox binary."
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
				Description: b.saDescription(),
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
				return fmt.Errorf("%w\nthe role ID %s was deleted more than 7 days ago and cannot be reused for 37 days; destroy --all deleted it, and only `devbox up` within 7 days can undelete it", err, config.SuspendRoleID)
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
func (b *Box) missingProjectBindings(pol *crm.Policy) []string {
	var miss []string
	for _, r := range agentRoles {
		if !hasMember(pol, r, b.C.SAMember(), nil) {
			miss = append(miss, r)
		}
	}
	return miss
}

// hasProjectSecretGrant finds secretAccessor granted on the whole project, with
// or without a condition. Earlier versions granted it conditioned on the devbox-
// name prefix, which every box in the project matched (devbox- is a prefix of
// devbox-test-), so one box could read another's machine key. Access is now
// granted on each secret instead, and this grant is revoked.
func (b *Box) hasProjectSecretGrant(pol *crm.Policy) bool {
	for _, bd := range pol.Bindings {
		if bd.Role == secretRole && slices.Contains(bd.Members, b.C.SAMember()) {
			return true
		}
	}
	return false
}

func (b *Box) planProjectIAM(ctx context.Context, p *Plan) error {
	pol, err := b.getProjectPolicy(ctx)
	if err != nil {
		return fmt.Errorf("project IAM policy: %w", err)
	}
	miss := b.missingProjectBindings(pol)
	revoke := b.hasProjectSecretGrant(pol)
	if len(miss) == 0 && !revoke {
		return nil
	}
	var what []string
	if len(miss) > 0 {
		what = append(what, fmt.Sprintf("grant %v", miss))
	}
	if revoke {
		what = append(what, "revoke project-wide "+secretRole)
	}
	p.add(Op{Resource: "project IAM", Action: "update", Detail: strings.Join(what, "; "), Run: func(ctx context.Context) error {
		return b.editProjectPolicy(ctx, func(pol *crm.Policy) {
			for _, r := range agentRoles {
				addMember(pol, r, b.C.SAMember(), nil)
			}
			removeMember(pol, b.C.SAMember(), secretRole)
		})
	}})
	return nil
}

// --- per-secret grants ---

// The Tailscale OAuth secret mints keys that join any device to the tailnet, and
// provision.sh needs it only once: to join a disk that has no Tailscale state.
// So the box never holds standing access to it. When the code disk is created,
// or on `devbox reprovision --rejoin`, it gets an IAM grant that expires on its
// own after tailnetJoinWindow.
const (
	tailnetJoinWindow = time.Hour
	joinCondTitle     = "devbox-tailnet-join"
)

// boxSecretIDs are the secrets this box reads at any time: the machine key, the
// sudo password hash if there is one, and every secret labelled devbox=<name>,
// which is how `devbox project secret` stores project secrets. The Tailscale
// OAuth secret is not one of them.
func (b *Box) boxSecretIDs(ctx context.Context) ([]string, error) {
	ids := []string{b.C.SSHKey.Secret}
	if b.C.Sudo.PasswordSecret != "" {
		ids = append(ids, b.C.Sudo.PasswordSecret)
	}
	labelled, err := b.ListLabelledSecrets(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range labelled {
		if id := gcp.Last(s.Name); !slices.Contains(ids, id) && id != b.C.Tailscale.OAuthSecret {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func secretGranted(pol *secretmanager.Policy, member string) bool {
	for _, bd := range pol.Bindings {
		if bd.Role == secretRole && bd.Condition == nil && slices.Contains(bd.Members, member) {
			return true
		}
	}
	return false
}

// getSecretPolicy reads at version 3, the only version that shows conditions.
func (b *Box) getSecretPolicy(ctx context.Context, id string) (*secretmanager.Policy, error) {
	return b.G.Secrets.Projects.Secrets.GetIamPolicy(b.secretName(id)).OptionsRequestedPolicyVersion(3).Context(ctx).Do()
}

// planSecretIAM grants the box's service account secretAccessor on each of its
// own secrets and nothing else, and takes away standing access to the Tailscale
// OAuth secret that earlier versions granted. A missing secret is checkSecrets'
// to report.
func (b *Box) planSecretIAM(ctx context.Context, p *Plan) error {
	ids, err := b.boxSecretIDs(ctx)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	for _, id := range ids {
		pol, err := b.getSecretPolicy(ctx, id)
		if gcp.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("secret %s IAM policy: %w", id, err)
		}
		if secretGranted(pol, b.C.SAMember()) {
			continue
		}
		p.add(Op{Resource: "secret " + id, Action: "update", Detail: "let " + b.C.Name + " read it", Run: func(ctx context.Context) error {
			return b.grantSecret(ctx, id)
		}})
	}
	ts := b.C.Tailscale.OAuthSecret
	pol, err := b.getSecretPolicy(ctx, ts)
	if gcp.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("secret %s IAM policy: %w", ts, err)
	}
	if secretGranted(pol, b.C.SAMember()) {
		p.add(Op{Resource: "secret " + ts, Action: "update", Detail: "revoke " + b.C.Name + "'s standing read; it gets one for an hour when it joins the tailnet", Run: func(ctx context.Context) error {
			return b.editSecretPolicy(ctx, ts, func(pol *secretmanager.Policy) bool {
				return dropSecretMember(pol, b.C.SAMember(), func(bd *secretmanager.Binding) bool { return bd.Condition == nil })
			})
		}})
	}
	return nil
}

// joinOp lets the box read the Tailscale OAuth secret for tailnetJoinWindow.
func (b *Box) joinOp() Op {
	return Op{Resource: "secret " + b.C.Tailscale.OAuthSecret, Action: "update",
		Detail: fmt.Sprintf("let %s read it for %s, to join the tailnet", b.C.Name, tailnetJoinWindow),
		Run:    b.GrantTailnetJoin}
}

// GrantTailnetJoin gives the box read access to the Tailscale OAuth secret until
// tailnetJoinWindow from now, replacing any earlier window or standing grant.
func (b *Box) GrantTailnetJoin(ctx context.Context) error {
	until := time.Now().Add(tailnetJoinWindow).UTC().Format(time.RFC3339)
	member := b.C.SAMember()
	return b.editSecretPolicy(ctx, b.C.Tailscale.OAuthSecret, func(pol *secretmanager.Policy) bool {
		dropSecretMember(pol, member, func(bd *secretmanager.Binding) bool {
			return bd.Condition == nil || bd.Condition.Title == joinCondTitle
		})
		pol.Bindings = append(pol.Bindings, &secretmanager.Binding{Role: secretRole, Members: []string{member}, Condition: &secretmanager.Expr{
			Title:       joinCondTitle,
			Description: "devbox " + b.C.Name + ": join the tailnet once, then expire",
			Expression:  fmt.Sprintf("request.time < timestamp(%q)", until),
		}})
		return true
	})
}

// dropSecretMember removes member from the secretRole bindings that match, and
// any binding that leaves empty.
func dropSecretMember(pol *secretmanager.Policy, member string, match func(*secretmanager.Binding) bool) bool {
	changed := false
	pol.Bindings = slices.DeleteFunc(pol.Bindings, func(bd *secretmanager.Binding) bool {
		if bd.Role != secretRole || !match(bd) {
			return false
		}
		n := len(bd.Members)
		bd.Members = slices.DeleteFunc(bd.Members, func(m string) bool { return m == member })
		changed = changed || len(bd.Members) != n
		return len(bd.Members) == 0
	})
	return changed
}

// grantSecret gives the box standing read access to one secret.
func (b *Box) grantSecret(ctx context.Context, id string) error {
	return b.editSecretPolicy(ctx, id, func(pol *secretmanager.Policy) bool {
		if secretGranted(pol, b.C.SAMember()) {
			return false
		}
		for _, bd := range pol.Bindings {
			if bd.Role == secretRole && bd.Condition == nil {
				bd.Members = append(bd.Members, b.C.SAMember())
				return true
			}
		}
		pol.Bindings = append(pol.Bindings, &secretmanager.Binding{Role: secretRole, Members: []string{b.C.SAMember()}})
		return true
	})
}

// editSecretPolicy is a read-modify-write of one secret's IAM policy, retried
// on etag races and on a service account IAM cannot see yet. edit reports
// whether it changed anything.
func (b *Box) editSecretPolicy(ctx context.Context, id string, edit func(*secretmanager.Policy) bool) error {
	retryable := func(err error) bool { c := gcp.Code(err); return c == 409 || c == 400 || c == 412 }
	return gcp.Retry(ctx, 90*time.Second, retryable, func() error {
		pol, err := b.getSecretPolicy(ctx, id)
		if err != nil {
			return err
		}
		if !edit(pol) {
			return nil
		}
		pol.Version = 3 // conditions are lost at any lower version
		_, err = b.G.Secrets.Projects.Secrets.SetIamPolicy(b.secretName(id), &secretmanager.SetIamPolicyRequest{Policy: pol}).Context(ctx).Do()
		return err
	})
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
