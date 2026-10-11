package box

import (
	"context"
	"fmt"
	"strings"
	"time"

	compute "google.golang.org/api/compute/v1"

	"github.com/Cool-Consulting-LLC/devbox/internal/assets"
	"github.com/Cool-Consulting-LLC/devbox/internal/gcp"
)

const (
	imageFamily = "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64"
	codeDevice  = "code"

	mdOSLogin   = "enable-oslogin"
	mdUserData  = "user-data"
	mdConfig    = "devbox-config"
	mdProvision = "devbox-provision"
)

// GetInstance returns the box's instance, nil if there is none, or the instance
// with an ErrNotOurs error if one by that name exists without this box's label.
func (b *Box) GetInstance(ctx context.Context) (*compute.Instance, error) {
	in, err := b.G.Compute.Instances.Get(b.C.ProjectID, b.C.Zone, b.C.Name).Context(ctx).Do()
	if gcp.IsNotFound(err) {
		return nil, nil
	}
	if err == nil && !b.ownsLabels(in.Labels) {
		return in, b.notOurs("instance", b.C.Name, b.labelFix("instances", b.C.Name+" --zone "+b.C.Zone))
	}
	return in, err
}

// desiredMetadata. Only user-data is replace tier; the rest is updated in place.
func (b *Box) desiredMetadata() map[string]string {
	return map[string]string{
		mdOSLogin:   "TRUE",
		mdUserData:  assets.CloudInit(),
		mdConfig:    assets.ConfigEnv(b.C),
		mdProvision: assets.Provision(),
	}
}

func metadataMap(m *compute.Metadata) map[string]string {
	out := map[string]string{}
	if m == nil {
		return out
	}
	for _, it := range m.Items {
		if it.Value != nil {
			out[it.Key] = *it.Value
		}
	}
	return out
}

func (b *Box) machineTypeURL() string {
	return fmt.Sprintf("zones/%s/machineTypes/%s", b.C.Zone, b.C.MachineType)
}

func (b *Box) desiredInstance() *compute.Instance {
	var items []*compute.MetadataItems
	for k, v := range b.desiredMetadata() {
		items = append(items, &compute.MetadataItems{Key: k, Value: &v})
	}
	nic := &compute.NetworkInterface{Network: b.networkURL(), Subnetwork: b.subnetURL()}
	if b.C.WantExternalIP() {
		nic.AccessConfigs = []*compute.AccessConfig{{Name: "external-nat", Type: "ONE_TO_ONE_NAT", NetworkTier: "PREMIUM"}}
	}
	return &compute.Instance{
		Name:        b.C.Name,
		Description: "devbox " + b.C.Name + ". Cattle: /srv/code is the only thing that survives a replacement.",
		MachineType: b.machineTypeURL(),
		Labels:      b.C.Labels(),
		Disks: []*compute.AttachedDisk{
			{
				Boot: true, AutoDelete: true, DeviceName: "boot",
				InitializeParams: &compute.AttachedDiskInitializeParams{
					SourceImage: imageFamily,
					DiskSizeGb:  b.C.BootDiskGB,
					DiskType:    fmt.Sprintf("zones/%s/diskTypes/%s", b.C.Zone, diskType),
					Labels:      b.C.Labels(),
				},
			},
			{
				AutoDelete: false, DeviceName: codeDevice, Mode: "READ_WRITE",
				Source: fmt.Sprintf("projects/%s/zones/%s/disks/%s", b.C.ProjectID, b.C.Zone, b.C.CodeDiskName()),
			},
		},
		NetworkInterfaces: []*compute.NetworkInterface{nic},
		ShieldedInstanceConfig: &compute.ShieldedInstanceConfig{
			EnableSecureBoot: true, EnableVtpm: true, EnableIntegrityMonitoring: true,
		},
		// cloud-platform is not optional: default scopes exclude compute, and a box
		// without it cannot suspend itself whatever IAM says.
		ServiceAccounts: []*compute.ServiceAccount{{Email: b.C.SAEmail(), Scopes: []string{scope}}},
		Metadata:        &compute.Metadata{Items: items},
		Scheduling:      &compute.Scheduling{OnHostMaintenance: "MIGRATE", AutomaticRestart: ptr(true)},
	}
}

const scope = "https://www.googleapis.com/auth/cloud-platform"

func ptr[T any](v T) *T { return &v }

func suspended(status string) bool { return status == "SUSPENDED" || status == "SUSPENDING" }

func (b *Box) planInstance(ctx context.Context, p *Plan, mode Mode) error {
	in, err := b.GetInstance(ctx)
	if err != nil {
		return fmt.Errorf("instance: %w", err)
	}
	res := "instance " + b.C.Name
	if in == nil {
		p.add(Op{Resource: res, Action: "create", Detail: fmt.Sprintf("%s, %s, %d GB boot", b.C.MachineType, gcp.Last(imageFamily), b.C.BootDiskGB), Run: b.createInstance})
		return nil
	}

	var changes []Op
	want := b.desiredMetadata()
	have := metadataMap(in.Metadata)

	nic := in.NetworkInterfaces[0]
	replaceWhy := []string{}
	if have[mdUserData] != want[mdUserData] {
		replaceWhy = append(replaceWhy, "cloud-init changed")
	}
	if gcp.Last(nic.Subnetwork) != b.C.Network.Subnet || gcp.Last(nic.Network) != b.C.Network.Name {
		replaceWhy = append(replaceWhy, "network/subnet changed")
	}
	if len(replaceWhy) > 0 {
		changes = append(changes, Op{Resource: res, Action: "replace", Disruptive: true,
			Detail: strings.Join(replaceWhy, ", ") + "; /srv/code survives, home directories and sessions do not",
			Run:    b.replaceInstance})
	} else {
		restartWhy := []string{}
		if gcp.Last(in.MachineType) != b.C.MachineType {
			restartWhy = append(restartWhy, fmt.Sprintf("machine type %s -> %s", gcp.Last(in.MachineType), b.C.MachineType))
		}
		if len(in.ServiceAccounts) != 1 || in.ServiceAccounts[0].Email != b.C.SAEmail() || !hasScope(in.ServiceAccounts[0].Scopes) {
			restartWhy = append(restartWhy, "service account")
		}
		if s := in.ShieldedInstanceConfig; s == nil || !s.EnableSecureBoot || !s.EnableVtpm || !s.EnableIntegrityMonitoring {
			restartWhy = append(restartWhy, "shielded VM config")
		}
		if len(restartWhy) > 0 {
			changes = append(changes, Op{Resource: res, Action: "restart", Disruptive: true,
				Detail: strings.Join(restartWhy, ", ") + "; disks survive, memory state does not",
				Run:    b.restartInstance})
		}

		var mdWhy []string
		for _, k := range []string{mdOSLogin, mdConfig, mdProvision} {
			if have[k] != want[k] {
				mdWhy = append(mdWhy, k)
			}
		}
		if len(mdWhy) > 0 {
			p.NeedsReprovision = p.NeedsReprovision || len(mdWhy) > 1 || mdWhy[0] != mdOSLogin
			changes = append(changes, Op{Resource: res, Action: "update", Detail: "metadata " + strings.Join(mdWhy, ", "), Run: b.setMetadata})
		}

		hasIP := len(nic.AccessConfigs) > 0
		if hasIP != b.C.WantExternalIP() {
			changes = append(changes, Op{Resource: res, Action: "update", Detail: fmt.Sprintf("external IP %v -> %v", hasIP, b.C.WantExternalIP()), Run: b.setAccessConfig})
		}

		if boot := bootDisk(in); boot != nil && boot.DiskSizeGb < b.C.BootDiskGB {
			p.NeedsReprovision = true
			changes = append(changes, Op{Resource: res, Action: "update", Detail: fmt.Sprintf("grow boot disk %d -> %d GB", boot.DiskSizeGb, b.C.BootDiskGB), Run: func(ctx context.Context) error {
				return b.G.Await(ctx)(b.G.Compute.Disks.Resize(b.C.ProjectID, b.C.Zone, gcp.Last(boot.Source),
					&compute.DisksResizeRequest{SizeGb: b.C.BootDiskGB}).Context(ctx).Do())
			}})
		} else if boot != nil && boot.DiskSizeGb > b.C.BootDiskGB {
			p.warn("boot_disk_gb is %d but the boot disk is %d GB; disks only grow", b.C.BootDiskGB, boot.DiskSizeGb)
		}

		if !hasCodeDisk(in, b.C.CodeDiskName()) {
			changes = append(changes, Op{Resource: res, Action: "update", Detail: "attach " + b.C.CodeDiskName(), Run: b.attachCodeDisk})
		}
	}
	// Every change starts from a running instance: never stop or edit a suspended one.
	switch {
	case suspended(in.Status) && (len(changes) > 0 || mode == ModeUp):
		p.add(Op{Resource: res, Action: "resume", Detail: "memory state restored", Run: b.Resume})
	case in.Status == "TERMINATED" || in.Status == "STOPPED" || in.Status == "STOPPING":
		p.add(Op{Resource: res, Action: "start", Run: b.start})
	}
	for _, o := range changes {
		p.add(o)
	}
	return nil
}

func hasScope(s []string) bool {
	for _, x := range s {
		if x == scope {
			return true
		}
	}
	return false
}

func bootDisk(in *compute.Instance) *compute.AttachedDisk {
	for _, d := range in.Disks {
		if d.Boot {
			return d
		}
	}
	return nil
}

func hasCodeDisk(in *compute.Instance, name string) bool {
	for _, d := range in.Disks {
		if gcp.Last(d.Source) == name {
			return true
		}
	}
	return false
}

func (b *Box) createInstance(ctx context.Context) error {
	// A brand-new service account can take a while to be attachable.
	return gcp.Retry(ctx, 90*time.Second, func(err error) bool { return gcp.Code(err) == 400 && strings.Contains(err.Error(), "serviceAccount") }, func() error {
		return b.G.Await(ctx)(b.G.Compute.Instances.Insert(b.C.ProjectID, b.C.Zone, b.desiredInstance()).Context(ctx).Do())
	})
}

func (b *Box) replaceInstance(ctx context.Context) error {
	in, err := b.GetInstance(ctx)
	if err != nil {
		return err
	}
	if in != nil {
		if suspended(in.Status) {
			if err := b.Resume(ctx); err != nil {
				return err
			}
		}
		b.logf("deleting instance; the code disk is kept (auto-delete off)")
		if err := b.G.Await(ctx)(b.G.Compute.Instances.Delete(b.C.ProjectID, b.C.Zone, b.C.Name).Context(ctx).Do()); err != nil {
			return err
		}
	}
	b.logf("creating instance")
	return b.createInstance(ctx)
}

func (b *Box) restartInstance(ctx context.Context) error {
	if err := b.waitStatus(ctx, "STOPPING"); err != nil {
		return err
	}
	in, err := b.GetInstance(ctx)
	if err != nil {
		return err
	}
	if suspended(in.Status) {
		if err := b.Resume(ctx); err != nil {
			return err
		}
	}
	if in.Status != "TERMINATED" {
		b.logf("stopping")
		if err := b.G.Await(ctx)(b.G.Compute.Instances.Stop(b.C.ProjectID, b.C.Zone, b.C.Name).Context(ctx).Do()); err != nil {
			return err
		}
	}
	if gcp.Last(in.MachineType) != b.C.MachineType {
		b.logf("machine type -> %s", b.C.MachineType)
		if err := b.G.Await(ctx)(b.G.Compute.Instances.SetMachineType(b.C.ProjectID, b.C.Zone, b.C.Name,
			&compute.InstancesSetMachineTypeRequest{MachineType: b.machineTypeURL()}).Context(ctx).Do()); err != nil {
			return err
		}
	}
	if err := b.G.Await(ctx)(b.G.Compute.Instances.SetServiceAccount(b.C.ProjectID, b.C.Zone, b.C.Name,
		&compute.InstancesSetServiceAccountRequest{Email: b.C.SAEmail(), Scopes: []string{scope}}).Context(ctx).Do()); err != nil {
		return err
	}
	if err := b.G.Await(ctx)(b.G.Compute.Instances.UpdateShieldedInstanceConfig(b.C.ProjectID, b.C.Zone, b.C.Name,
		&compute.ShieldedInstanceConfig{EnableSecureBoot: true, EnableVtpm: true, EnableIntegrityMonitoring: true}).Context(ctx).Do()); err != nil {
		return err
	}
	return b.start(ctx)
}

// setMetadata replaces the in-place managed keys and keeps every other key.
func (b *Box) setMetadata(ctx context.Context) error {
	in, err := b.GetInstance(ctx)
	if err != nil {
		return err
	}
	want := b.desiredMetadata()
	// Never rewrite user-data in place: the running box would not apply it, and
	// apply would stop seeing the difference that means "replace".
	delete(want, mdUserData)
	md := &compute.Metadata{Fingerprint: in.Metadata.Fingerprint}
	for _, it := range in.Metadata.Items {
		if _, managed := want[it.Key]; !managed {
			md.Items = append(md.Items, it)
		}
	}
	for k, v := range want {
		md.Items = append(md.Items, &compute.MetadataItems{Key: k, Value: &v})
	}
	return b.G.Await(ctx)(b.G.Compute.Instances.SetMetadata(b.C.ProjectID, b.C.Zone, b.C.Name, md).Context(ctx).Do())
}

func (b *Box) setAccessConfig(ctx context.Context) error {
	in, err := b.GetInstance(ctx)
	if err != nil {
		return err
	}
	nic := in.NetworkInterfaces[0]
	if b.C.WantExternalIP() {
		return b.G.Await(ctx)(b.G.Compute.Instances.AddAccessConfig(b.C.ProjectID, b.C.Zone, b.C.Name, nic.Name,
			&compute.AccessConfig{Name: "external-nat", Type: "ONE_TO_ONE_NAT", NetworkTier: "PREMIUM"}).Context(ctx).Do())
	}
	for _, ac := range nic.AccessConfigs {
		if err := b.G.Await(ctx)(b.G.Compute.Instances.DeleteAccessConfig(b.C.ProjectID, b.C.Zone, b.C.Name, ac.Name, nic.Name).Context(ctx).Do()); err != nil {
			return err
		}
	}
	return nil
}

func (b *Box) attachCodeDisk(ctx context.Context) error {
	return b.G.Await(ctx)(b.G.Compute.Instances.AttachDisk(b.C.ProjectID, b.C.Zone, b.C.Name, &compute.AttachedDisk{
		DeviceName: codeDevice, Mode: "READ_WRITE", AutoDelete: false,
		Source: fmt.Sprintf("projects/%s/zones/%s/disks/%s", b.C.ProjectID, b.C.Zone, b.C.CodeDiskName()),
	}).Context(ctx).Do())
}

// Resume restores a suspended instance, waiting out SUSPENDING first.
func (b *Box) Resume(ctx context.Context) error {
	if err := b.waitStatus(ctx, "SUSPENDING"); err != nil {
		return err
	}
	return b.G.Await(ctx)(b.G.Compute.Instances.Resume(b.C.ProjectID, b.C.Zone, b.C.Name).Context(ctx).Do())
}

func (b *Box) start(ctx context.Context) error {
	if err := b.waitStatus(ctx, "STOPPING"); err != nil {
		return err
	}
	return b.G.Await(ctx)(b.G.Compute.Instances.Start(b.C.ProjectID, b.C.Zone, b.C.Name).Context(ctx).Do())
}

// Suspend preserves guest memory; tmux and running sessions survive.
func (b *Box) Suspend(ctx context.Context) error {
	return b.G.Await(ctx)(b.G.Compute.Instances.Suspend(b.C.ProjectID, b.C.Zone, b.C.Name).Context(ctx).Do())
}

// waitStatus polls while the instance is in a transitional status.
func (b *Box) waitStatus(ctx context.Context, transitional string) error {
	for {
		in, err := b.GetInstance(ctx)
		if err != nil || in == nil || in.Status != transitional {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// SyncProvisionMetadata pushes the reprovision-tier metadata (config and
// provision.sh) without touching anything else. Used by `reprovision`.
func (b *Box) SyncProvisionMetadata(ctx context.Context) (*compute.Instance, error) {
	in, err := b.GetInstance(ctx)
	if err != nil || in == nil {
		return in, err
	}
	have, want := metadataMap(in.Metadata), b.desiredMetadata()
	if have[mdConfig] == want[mdConfig] && have[mdProvision] == want[mdProvision] && have[mdOSLogin] == want[mdOSLogin] {
		return in, nil
	}
	if have[mdUserData] != want[mdUserData] {
		b.logf("note: cloud-init also changed; that needs `devbox apply` (replaces the instance)")
	}
	b.logf("updating instance metadata (devbox-config, devbox-provision)")
	return in, b.setMetadata(ctx)
}
