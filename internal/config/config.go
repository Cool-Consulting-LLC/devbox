// Package config loads and validates devbox.yaml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Change tiers, as described in README.md, Changing things.
const (
	TierIdentity    = "identity"    // addresses a different box; not a change to this one
	TierInPlace     = "in-place"    // `apply` updates GCP resources; the box is untouched
	TierReprovision = "reprovision" // `apply` updates metadata; `reprovision` applies it on the box
	TierRestart     = "restart"     // `apply` stops and starts the instance; memory state is lost
	TierReplace     = "replace"     // `apply` deletes and recreates the instance; /srv/code survives
	TierRefused     = "refused"     // not implemented; setting it is an error
)

// SecretPrefix is the Secret Manager name prefix the box can read, for every box.
const SecretPrefix = "devbox-"

// Config is devbox.yaml. Struct tags drive both parsing and `devbox config --describe`.
type Config struct {
	ProjectID     string `yaml:"project_id" describe:"GCP project to build in. You need owner on it, or editor plus Project IAM Admin and Role Administrator. Exactly one of this or create_project." tier:"identity"`
	CreateProject bool   `yaml:"create_project" describe:"Create the project, VPC and subnet. Backlogged: true is refused today." tier:"refused" default:"false"`

	Zone string `yaml:"zone" describe:"Compute zone. The region is derived from it and must match the subnet's region." tier:"identity" default:"europe-west4-a"`
	Name string `yaml:"name" describe:"Box name. Every resource is named after it (NAME-code, NAME-allow-iap-ssh, ...) and labelled devbox=NAME. Also the Tailscale hostname and the service account ID. 6-30 chars, lowercase, digits, hyphens." tier:"identity" default:"devbox"`

	Network Network `yaml:"network"`

	MachineType string `yaml:"machine_type" describe:"Compute machine type. Must be suspend-capable; E2 is the only family that reports guest memory without an agent." tier:"restart" default:"e2-standard-2"`
	ExternalIP  *bool  `yaml:"external_ip" describe:"Give the instance an ephemeral external IP for egress. Set false only if the subnet has Cloud NAT and Private Google Access." tier:"in-place" default:"true"`
	CodeDiskGB  int64  `yaml:"code_disk_gb" describe:"Size of the /srv/code pet disk. Grow only; shrinking is refused." tier:"reprovision" default:"50"`
	BootDiskGB  int64  `yaml:"boot_disk_gb" describe:"Size of the boot disk. Grow only; shrinking is refused." tier:"reprovision" default:"30"`

	ExtraPackages []string `yaml:"extra_packages" describe:"Extra apt packages, usually language runtimes, installed by provision.sh." tier:"reprovision" default:"[]"`

	Git       Git         `yaml:"git"`
	Tailscale Tailscale   `yaml:"tailscale"`
	SSHKey    SSHKey      `yaml:"ssh_key"`
	Snapshots Snapshots   `yaml:"snapshots"`
	Alerts    Alerts      `yaml:"alerts"`
	Idle      IdleSuspend `yaml:"idle_suspend"`
	Glass     BreakGlass  `yaml:"break_glass"`
}

type Network struct {
	Name   string `yaml:"name" describe:"An existing VPC network in the project. Never created or deleted." tier:"replace" default:"default"`
	Subnet string `yaml:"subnet" describe:"An existing subnet of that network, in the zone's region." tier:"replace" default:"default"`
}

type Git struct {
	Name  string `yaml:"name" describe:"user.name in the rendered /etc/gitconfig." tier:"reprovision"`
	Email string `yaml:"email" describe:"user.email in /etc/gitconfig and /etc/devbox/allowed_signers." tier:"reprovision"`
}

type Tailscale struct {
	Tag         string `yaml:"tag" describe:"ACL tag the node advertises. Must be owned by the OAuth client." tier:"reprovision" default:"tag:devbox"`
	OAuthSecret string `yaml:"oauth_secret" describe:"Secret Manager secret holding the Tailscale OAuth client secret. Must start with devbox-." tier:"reprovision" default:"devbox-tailscale-oauth"`
}

type SSHKey struct {
	Secret string `yaml:"secret" describe:"Secret Manager secret holding the machine ed25519 private key, loaded into the system ssh-agent at boot. Must start with devbox-." tier:"reprovision" default:"devbox-ssh-key"`
}

type Snapshots struct {
	StartTime     string `yaml:"start_time" describe:"Daily snapshot time of the code disk, UTC, on the hour (HH:00)." tier:"in-place" default:"03:00"`
	RetentionDays int64  `yaml:"retention_days" describe:"Days each snapshot is kept. Snapshots outlive the disk." tier:"in-place" default:"14"`
}

type Alerts struct {
	Email       string `yaml:"email" describe:"Address of the email notification channel. Empty disables all alerting resources." tier:"in-place"`
	CPUHighPct  int    `yaml:"cpu_high_pct" describe:"Alert when CPU stays above this percentage for 5 minutes." tier:"in-place" default:"80"`
	CPUIdlePct  int    `yaml:"cpu_idle_pct" describe:"Alert when a running box stays below this percentage for 6 hours: something kept it from suspending, or the idle timer is broken." tier:"in-place" default:"5"`
	DiskUsedPct int    `yaml:"disk_used_pct" describe:"Alert when /srv/code is fuller than this percentage." tier:"in-place" default:"85"`
}

type IdleSuspend struct {
	Enabled      *bool `yaml:"enabled" describe:"Suspend the box after a period with no keyboard input, no Claude Code turn in progress and no non-interactive SSH session." tier:"reprovision" default:"true"`
	AfterMinutes int   `yaml:"after_minutes" describe:"Idle minutes before the box suspends itself." tier:"reprovision" default:"60"`
}

type BreakGlass struct {
	Enabled    bool   `yaml:"enabled" describe:"Create a firewall rule opening sshd to source_cidr, for when Tailscale and IAP are both unavailable." tier:"in-place" default:"false"`
	SourceCIDR string `yaml:"source_cidr" describe:"Your public address as /32 (IPv4) or /128 (IPv6). Required when enabled. 0.0.0.0/0 is refused." tier:"in-place"`
	AllowMosh  bool   `yaml:"allow_mosh" describe:"Also open udp:60000-61000 to source_cidr." tier:"in-place" default:"false"`
}

func boolPtr(b bool) *bool { return &b }

// Defaults returns a config with every default applied and nothing else.
func Defaults() *Config {
	return &Config{
		Zone:        "europe-west4-a",
		Name:        "devbox",
		Network:     Network{Name: "default", Subnet: "default"},
		MachineType: "e2-standard-2",
		ExternalIP:  boolPtr(true),
		CodeDiskGB:  50,
		BootDiskGB:  30,
		Tailscale:   Tailscale{Tag: "tag:devbox", OAuthSecret: "devbox-tailscale-oauth"},
		SSHKey:      SSHKey{Secret: "devbox-ssh-key"},
		Snapshots:   Snapshots{StartTime: "03:00", RetentionDays: 14},
		Alerts:      Alerts{CPUHighPct: 80, CPUIdlePct: 5, DiskUsedPct: 85},
		Idle:        IdleSuspend{Enabled: boolPtr(true), AfterMinutes: 60},
	}
}

// Load reads path, applies defaults for absent keys and validates.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse decodes YAML over the defaults, rejecting unknown keys, and validates.
func Parse(data []byte) (*Config, error) {
	c := Defaults()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	// yaml.v3 overwrites pointers with nil on explicit null; restore defaults.
	if c.ExternalIP == nil {
		c.ExternalIP = boolPtr(true)
	}
	if c.Idle.Enabled == nil {
		c.Idle.Enabled = boolPtr(true)
	}
	return c, c.Validate()
}

var (
	nameRe     = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	zoneRe     = regexp.MustCompile(`^([a-z]+-[a-z]+[0-9]+)-[a-z]$`)
	projectRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	hourRe     = regexp.MustCompile(`^([01][0-9]|2[0-3]):00$`)
	tagRe      = regexp.MustCompile(`^tag:[a-z0-9-]+$`)
	secretIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
	pkgRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+(=[A-Za-z0-9.+~:-]+)?$`)
)

// Validate returns every problem at once, one per line.
func (c *Config) Validate() error {
	var errs []string
	bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	switch {
	case c.CreateProject && c.ProjectID != "":
		bad("set exactly one of project_id or create_project, not both")
	case c.CreateProject:
		bad("create_project is backlogged and refused today; create the project yourself and set project_id")
	case c.ProjectID == "":
		bad("project_id is required")
	case !projectRe.MatchString(c.ProjectID):
		bad("project_id %q is not a valid project ID", c.ProjectID)
	}
	if !nameRe.MatchString(c.Name) {
		bad("name %q must be 6-30 characters of lowercase letters, digits and hyphens, starting with a letter (it is also the service account ID)", c.Name)
	}
	if !zoneRe.MatchString(c.Zone) {
		bad("zone %q does not look like a zone, e.g. europe-west4-a", c.Zone)
	}
	if c.Network.Name == "" || c.Network.Subnet == "" {
		bad("network.name and network.subnet are both required")
	}
	if c.MachineType == "" {
		bad("machine_type is required")
	}
	if c.CodeDiskGB < 10 {
		bad("code_disk_gb must be at least 10")
	}
	if c.BootDiskGB < 10 {
		bad("boot_disk_gb must be at least 10")
	}
	for _, p := range c.ExtraPackages {
		if !pkgRe.MatchString(p) {
			bad("extra_packages: %q is not an apt package name", p)
		}
	}
	if c.Git.Name == "" || c.Git.Email == "" || !strings.Contains(c.Git.Email, "@") {
		bad("git.name and git.email are required")
	}
	if !tagRe.MatchString(c.Tailscale.Tag) {
		bad("tailscale.tag %q must look like tag:devbox", c.Tailscale.Tag)
	}
	for k, v := range map[string]string{"tailscale.oauth_secret": c.Tailscale.OAuthSecret, "ssh_key.secret": c.SSHKey.Secret} {
		if !strings.HasPrefix(v, SecretPrefix) || !secretIDRe.MatchString(v) {
			bad("%s %q must be a secret ID starting with %q, or the box cannot read it", k, v, SecretPrefix)
		}
	}
	if !hourRe.MatchString(c.Snapshots.StartTime) {
		bad("snapshots.start_time %q must be HH:00 in UTC", c.Snapshots.StartTime)
	}
	if c.Snapshots.RetentionDays < 1 {
		bad("snapshots.retention_days must be at least 1")
	}
	if c.Alerts.Email != "" && !strings.Contains(c.Alerts.Email, "@") {
		bad("alerts.email %q is not an email address", c.Alerts.Email)
	}
	for k, v := range map[string]int{"alerts.cpu_high_pct": c.Alerts.CPUHighPct, "alerts.cpu_idle_pct": c.Alerts.CPUIdlePct, "alerts.disk_used_pct": c.Alerts.DiskUsedPct} {
		if v < 1 || v > 99 {
			bad("%s must be between 1 and 99", k)
		}
	}
	if c.Alerts.CPUIdlePct >= c.Alerts.CPUHighPct {
		bad("alerts.cpu_idle_pct must be below alerts.cpu_high_pct")
	}
	if c.Idle.AfterMinutes < 5 {
		bad("idle_suspend.after_minutes must be at least 5 (the check runs every 5 minutes)")
	}
	if c.Glass.Enabled {
		if err := checkCIDR(c.Glass.SourceCIDR); err != nil {
			bad("break_glass.source_cidr: %v", err)
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n"))
	}
	return nil
}

func checkCIDR(s string) error {
	if s == "" {
		return errors.New("required when break_glass.enabled is true; use your public IP as /32")
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return fmt.Errorf("%q is not a CIDR", s)
	}
	if p.Bits() == 0 {
		return fmt.Errorf("%q opens sshd to the whole internet; refused", s)
	}
	if p != p.Masked() {
		return fmt.Errorf("%q has host bits set; did you mean %s?", s, p.Masked())
	}
	if (p.Addr().Is4() && p.Bits() < 24) || (p.Addr().Is6() && p.Bits() < 48) {
		return fmt.Errorf("%q is wider than a /24 (IPv4) or /48 (IPv6); scope it to your own address", s)
	}
	return nil
}

// Region is derived from the zone.
func (c *Config) Region() string {
	m := zoneRe.FindStringSubmatch(c.Zone)
	if m == nil {
		return ""
	}
	return m[1]
}

func (c *Config) WantExternalIP() bool { return c.ExternalIP == nil || *c.ExternalIP }
func (c *Config) IdleEnabled() bool    { return c.Idle.Enabled == nil || *c.Idle.Enabled }

// Resource names. Everything is <name>-* so destroy can find what has no labels.
func (c *Config) CodeDiskName() string       { return c.Name + "-code" }
func (c *Config) SnapshotPolicyName() string { return c.Name + "-code-snapshots" }
func (c *Config) FirewallIAPName() string    { return c.Name + "-allow-iap-ssh" }
func (c *Config) FirewallGlassName() string  { return c.Name + "-break-glass" }
func (c *Config) SAEmail() string {
	return fmt.Sprintf("%s@%s.iam.gserviceaccount.com", c.Name, c.ProjectID)
}
func (c *Config) SAMember() string { return "serviceAccount:" + c.SAEmail() }

// Labels applied to everything that takes labels.
func (c *Config) Labels() map[string]string { return map[string]string{"devbox": c.Name} }

// LabelKey and LabelValue are the selector used to enumerate.
const LabelKey = "devbox"

// SuspendRoleID is shared by every box in the project: the role is identical for all
// of them, and a custom role ID cannot be reused for 37 days after deletion.
const SuspendRoleID = "devbox.selfSuspend"

func (c *Config) SuspendRoleName() string {
	return fmt.Sprintf("projects/%s/roles/%s", c.ProjectID, SuspendRoleID)
}
