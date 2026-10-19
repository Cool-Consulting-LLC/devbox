package box

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strings"

	secretmanager "google.golang.org/api/secretmanager/v1"

	"github.com/Cool-Consulting-LLC/devbox/internal/config"
	"github.com/Cool-Consulting-LLC/devbox/internal/gcp"
)

func (b *Box) secretName(id string) string {
	return fmt.Sprintf("projects/%s/secrets/%s", b.C.ProjectID, id)
}

// SecretExists reports whether the secret exists and has an enabled version.
func (b *Box) SecretExists(ctx context.Context, id string) (bool, error) {
	_, err := b.G.Secrets.Projects.Secrets.Versions.Get(b.secretName(id) + "/versions/latest").Context(ctx).Do()
	if gcp.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// Reconciling never creates the secrets the box needs, because only you hold
// their contents; `devbox secret` stores them. It checks for them.
func (b *Box) checkSecrets(ctx context.Context, p *Plan) error {
	ids := []string{b.C.Tailscale.OAuthSecret, b.C.SSHKey.Secret}
	if b.C.Sudo.PasswordSecret != "" {
		ids = append(ids, b.C.Sudo.PasswordSecret)
	}
	for _, id := range ids {
		ok, err := b.SecretExists(ctx, id)
		if err != nil {
			return fmt.Errorf("secret %s: %w", id, err)
		}
		if !ok {
			p.warn("secret %s is missing; provision.sh will fail until it exists (devbox secret --help)", id)
		}
	}
	return nil
}

// ListLabelledSecrets returns the secrets labelled devbox=<name>.
func (b *Box) ListLabelledSecrets(ctx context.Context) ([]*secretmanager.Secret, error) {
	var out []*secretmanager.Secret
	err := b.G.Secrets.Projects.Secrets.List("projects/"+b.C.ProjectID).
		Filter(fmt.Sprintf("labels.%s=%s", config.LabelKey, b.C.Name)).
		Pages(ctx, func(r *secretmanager.ListSecretsResponse) error {
			out = append(out, r.Secrets...)
			return nil
		})
	return out, err
}

var projectRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ProjectSecretID is the Secret Manager ID for a project's env var on box name.
// The box name leads, so two boxes with a project of the same name never share
// or overwrite each other's secret.
func ProjectSecretID(name, project, key string) (string, error) {
	if !projectRe.MatchString(project) {
		return "", fmt.Errorf("project %q must be the directory name under /srv/code", project)
	}
	if !envKeyRe.MatchString(key) {
		return "", fmt.Errorf("%q is not an environment variable name", key)
	}
	id := name + "-" + project + "-" + key
	if len(id) > 255 {
		return "", fmt.Errorf("secret ID %s is too long", id)
	}
	// Secret IDs allow letters, digits, - and _; project directories may contain dots.
	return regexp.MustCompile(`[^A-Za-z0-9_-]`).ReplaceAllString(id, "_"), nil
}

// PutSecret stores a project's secret and lets the box read it. Returns true
// when the secret already existed (a rotation).
func (b *Box) PutSecret(ctx context.Context, id, project string, value []byte) (bool, error) {
	existed, err := b.putSecret(ctx, id, map[string]string{config.LabelKey: b.C.Name, "devbox-project": labelValue(project)}, value)
	if err != nil {
		return existed, err
	}
	return existed, b.grantIfSA(ctx, id)
}

// PutBoxSecret stores one of the secrets devbox.yaml names: the Tailscale OAuth
// secret, the machine key or the sudo password hash. The box gets standing read
// access to each except the Tailscale one, which it reads only while joining.
func (b *Box) PutBoxSecret(ctx context.Context, id string, value []byte) (bool, error) {
	existed, err := b.putSecret(ctx, id, map[string]string{config.LabelKey: b.C.Name}, value)
	if err != nil || id == b.C.Tailscale.OAuthSecret {
		return existed, err
	}
	return existed, b.grantIfSA(ctx, id)
}

// putSecret creates the secret if needed and adds value as its newest version.
func (b *Box) putSecret(ctx context.Context, id string, labels map[string]string, value []byte) (bool, error) {
	_, err := b.G.Secrets.Projects.Secrets.Create("projects/"+b.C.ProjectID, &secretmanager.Secret{
		Labels:      labels,
		Replication: &secretmanager.Replication{Automatic: &secretmanager.Automatic{}},
	}).SecretId(id).Context(ctx).Do()
	existed := gcp.IsConflict(err)
	if err != nil && !existed {
		return false, err
	}
	_, err = b.G.Secrets.Projects.Secrets.AddVersion(b.secretName(id), &secretmanager.AddSecretVersionRequest{
		Payload: &secretmanager.SecretPayload{Data: base64.StdEncoding.EncodeToString(value)},
	}).Context(ctx).Do()
	if err != nil {
		return existed, err
	}
	if existed {
		b.disableOldVersions(ctx, id)
	}
	return existed, nil
}

// grantIfSA lets the box read the secret. Without its service account there is
// nobody to grant yet; `devbox up` grants the box's secrets when it creates one.
func (b *Box) grantIfSA(ctx context.Context, id string) error {
	if sa, err := b.getSA(ctx); err != nil || sa == nil {
		return err
	}
	return b.grantSecret(ctx, id)
}

// disableOldVersions leaves only the newest version enabled after a rotation. Best effort.
func (b *Box) disableOldVersions(ctx context.Context, id string) {
	var vs []*secretmanager.SecretVersion
	_ = b.G.Secrets.Projects.Secrets.Versions.List(b.secretName(id)).Filter("state:ENABLED").Pages(ctx, func(r *secretmanager.ListSecretVersionsResponse) error {
		vs = append(vs, r.Versions...)
		return nil
	})
	sort.Slice(vs, func(i, j int) bool { return vs[i].CreateTime > vs[j].CreateTime }) // RFC 3339 sorts as text
	for i, v := range vs {
		if i == 0 {
			continue
		}
		if _, err := b.G.Secrets.Projects.Secrets.Versions.Disable(v.Name, &secretmanager.DisableSecretVersionRequest{}).Context(ctx).Do(); err != nil {
			b.logf("could not disable old version %s: %v", v.Name, err)
		}
	}
}

// AccessSecret returns the value of the secret's newest enabled version.
func (b *Box) AccessSecret(ctx context.Context, id string) ([]byte, error) {
	r, err := b.G.Secrets.Projects.Secrets.Versions.Access(b.secretName(id) + "/versions/latest").Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(r.Payload.Data)
}

func (b *Box) DeleteSecret(ctx context.Context, id string) error {
	_, err := b.G.Secrets.Projects.Secrets.Delete(b.secretName(id)).Context(ctx).Do()
	if gcp.IsNotFound(err) {
		return nil
	}
	return err
}

var labelBad = regexp.MustCompile(`[^a-z0-9_-]`)

func labelValue(s string) string {
	v := labelBad.ReplaceAllString(strings.ToLower(s), "_")
	if len(v) > 63 {
		v = v[:63]
	}
	return v
}
