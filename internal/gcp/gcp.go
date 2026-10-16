// Package gcp holds the API clients and the small helpers every resource needs.
package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
	crm "google.golang.org/api/cloudresourcemanager/v1"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	iam "google.golang.org/api/iam/v1"
	monitoring "google.golang.org/api/monitoring/v3"
	oauth2api "google.golang.org/api/oauth2/v2"
	"google.golang.org/api/option"
	oslogin "google.golang.org/api/oslogin/v1"
	secretmanager "google.golang.org/api/secretmanager/v1"
	serviceusage "google.golang.org/api/serviceusage/v1"
)

const scope = "https://www.googleapis.com/auth/cloud-platform"

// ErrNoCredentials means there is no Google login at all.
var ErrNoCredentials = errors.New("no Google credentials")

// Clients is every API the binary speaks. Credentials come from Application Default
// Credentials: run `gcloud auth application-default login` once on the laptop.
type Clients struct {
	Project string

	Compute    *compute.Service
	IAM        *iam.Service
	CRM        *crm.Service
	Secrets    *secretmanager.Service
	Monitoring *monitoring.Service
	Usage      *serviceusage.Service
	OSLogin    *oslogin.Service

	creds *google.Credentials
	email string // fixed caller, for tests
}

func New(ctx context.Context, project string) (*Clients, error) {
	creds, err := google.FindDefaultCredentials(ctx, scope, "https://www.googleapis.com/auth/userinfo.email")
	if err != nil {
		return nil, fmt.Errorf("%w: run `gcloud auth application-default login` (%w)", ErrNoCredentials, err)
	}
	c, err := newClients(ctx, project, func(string) []option.ClientOption {
		return []option.ClientOption{option.WithCredentials(creds), option.WithQuotaProject(project)}
	})
	if err != nil {
		return nil, err
	}
	c.creds = creds
	return c, nil
}

// NewForTest points every client at base/<service>/ with no authentication.
func NewForTest(ctx context.Context, project, email, base string) (*Clients, error) {
	c, err := newClients(ctx, project, func(svc string) []option.ClientOption {
		return []option.ClientOption{option.WithEndpoint(base + "/" + svc + "/"), option.WithoutAuthentication()}
	})
	if err != nil {
		return nil, err
	}
	c.email = email
	return c, nil
}

func newClients(ctx context.Context, project string, opts func(svc string) []option.ClientOption) (*Clients, error) {
	c := &Clients{Project: project}
	var err error
	if c.Compute, err = compute.NewService(ctx, opts("compute")...); err != nil {
		return nil, err
	}
	if c.IAM, err = iam.NewService(ctx, opts("iam")...); err != nil {
		return nil, err
	}
	if c.CRM, err = crm.NewService(ctx, opts("crm")...); err != nil {
		return nil, err
	}
	if c.Secrets, err = secretmanager.NewService(ctx, opts("secretmanager")...); err != nil {
		return nil, err
	}
	if c.Monitoring, err = monitoring.NewService(ctx, opts("monitoring")...); err != nil {
		return nil, err
	}
	if c.Usage, err = serviceusage.NewService(ctx, opts("serviceusage")...); err != nil {
		return nil, err
	}
	if c.OSLogin, err = oslogin.NewService(ctx, opts("oslogin")...); err != nil {
		return nil, err
	}
	return c, nil
}

// CheckLogin mints a token, so an expired login fails here rather than halfway
// through a command.
func (c *Clients) CheckLogin() error {
	if c.creds == nil {
		return nil
	}
	if _, err := c.creds.TokenSource.Token(); err != nil {
		return fmt.Errorf("cannot mint a token: %w", err)
	}
	return nil
}

// CallerEmail is the account the binary is acting as.
func (c *Clients) CallerEmail(ctx context.Context) (string, error) {
	if c.email != "" {
		return c.email, nil
	}
	tok, err := c.creds.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("cannot mint a token: %w", err)
	}
	svc, err := oauth2api.NewService(ctx, option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		return "", err
	}
	info, err := svc.Tokeninfo().AccessToken(tok.AccessToken).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	if info.Email == "" {
		return "", errors.New("credentials carry no email; re-run `gcloud auth application-default login`")
	}
	return info.Email, nil
}

// UserCredentialJSON is the laptop's login as an authorized_user ADC file, or nil
// when the credentials are anything else: a service account, impersonation, or
// the metadata server.
func (c *Clients) UserCredentialJSON() []byte {
	if c.creds == nil || len(c.creds.JSON) == 0 {
		return nil
	}
	var f struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(c.creds.JSON, &f) != nil || f.Type != "authorized_user" {
		return nil
	}
	return c.creds.JSON
}

// Code returns the HTTP status of a Google API error, or 0.
func Code(err error) int {
	var g *googleapi.Error
	if errors.As(err, &g) {
		return g.Code
	}
	return 0
}

// NeedsLogin reports whether err means the laptop's credentials are missing,
// expired or revoked, e.g. by a Workspace reauthentication policy (invalid_rapt).
// It matches on text because the token error reaches callers through several
// wrappers.
func NeedsLogin(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNoCredentials) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "invalid_grant") || strings.Contains(s, "invalid_rapt") ||
		strings.Contains(s, "reauth")
}

func IsNotFound(err error) bool { return Code(err) == http.StatusNotFound }
func IsConflict(err error) bool { return Code(err) == http.StatusConflict }

// Wait blocks until a Compute operation finishes and returns its error, if any.
func (c *Clients) Wait(ctx context.Context, op *compute.Operation) error {
	for op.Status != "DONE" {
		var err error
		switch {
		case op.Zone != "":
			op, err = c.Compute.ZoneOperations.Wait(c.Project, last(op.Zone), op.Name).Context(ctx).Do()
		case op.Region != "":
			op, err = c.Compute.RegionOperations.Wait(c.Project, last(op.Region), op.Name).Context(ctx).Do()
		default:
			op, err = c.Compute.GlobalOperations.Wait(c.Project, op.Name).Context(ctx).Do()
		}
		if err != nil {
			return err
		}
	}
	if op.Error != nil && len(op.Error.Errors) > 0 {
		var msgs []string
		for _, e := range op.Error.Errors {
			msgs = append(msgs, e.Code+": "+e.Message)
		}
		return errors.New(strings.Join(msgs, "; "))
	}
	return nil
}

// Await takes a Compute call's results and waits for its operation:
// c.Await(ctx)(c.Compute.Instances.Start(...).Do()).
func (c *Clients) Await(ctx context.Context) func(*compute.Operation, error) error {
	return func(op *compute.Operation, err error) error {
		if err != nil {
			return err
		}
		return c.Wait(ctx, op)
	}
}

// Retry calls f until it succeeds, returns a non-retryable error, or time runs out.
// IAM is eventually consistent: a new service account is not instantly bindable.
func Retry(ctx context.Context, d time.Duration, retryable func(error) bool, f func() error) error {
	deadline := time.Now().Add(d)
	wait := time.Second
	for {
		err := f()
		if err == nil || !retryable(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		if wait < 8*time.Second {
			wait *= 2
		}
	}
}

func last(url string) string { return url[strings.LastIndex(url, "/")+1:] }

// Last is the final path segment of a self link.
func Last(url string) string { return last(url) }
