package box

// An in-memory fake of the GCP REST endpoints the binary calls. It enforces the
// rules the reconciler depends on (fingerprints, etags, resume-before-edit,
// stop-before-setMachineType, no deleting attached disks) so ordering bugs fail
// tests instead of real applies.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	crm "google.golang.org/api/cloudresourcemanager/v1"
	compute "google.golang.org/api/compute/v1"
	iam "google.golang.org/api/iam/v1"
	monitoring "google.golang.org/api/monitoring/v3"
	secretmanager "google.golang.org/api/secretmanager/v1"
)

const fakeProjectNumber = 123456789

type fakeSecret struct {
	s        *secretmanager.Secret
	versions []*secretmanager.SecretVersion
	data     map[string]string // version name -> base64 payload
	policy   *secretmanager.Policy
}

type fakeGCP struct {
	t  *testing.T
	mu sync.Mutex

	nextID     uint64
	fp         int
	calls      []string
	instances  map[string]*compute.Instance
	instIAM    map[string]*compute.Policy
	disks      map[string]*compute.Disk
	policies   map[string]*compute.ResourcePolicy
	firewalls  map[string]*compute.Firewall
	snapshots  map[string]*compute.Snapshot
	sas        map[string]*iam.ServiceAccount
	roles      map[string]*iam.Role
	projPolicy *crm.Policy
	secrets    map[string]*fakeSecret
	channels   map[string]*monitoring.NotificationChannel
	alerts     map[string]*monitoring.AlertPolicy
	uptimeSecs []float64 // points returned for any time series query

	srv *httptest.Server
}

func newFake(t *testing.T) *fakeGCP {
	f := &fakeGCP{
		t: t, nextID: 1000,
		instances: map[string]*compute.Instance{}, instIAM: map[string]*compute.Policy{},
		disks: map[string]*compute.Disk{}, policies: map[string]*compute.ResourcePolicy{},
		firewalls: map[string]*compute.Firewall{}, snapshots: map[string]*compute.Snapshot{},
		sas: map[string]*iam.ServiceAccount{}, roles: map[string]*iam.Role{},
		projPolicy: &crm.Policy{Etag: "e0", Version: 1, Bindings: []*crm.Binding{{Role: "roles/owner", Members: []string{"user:me@example.com"}}}},
		secrets:    map[string]*fakeSecret{},
		channels:   map[string]*monitoring.NotificationChannel{}, alerts: map[string]*monitoring.AlertPolicy{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

type apiErr struct {
	code int
	msg  string
}

func (e apiErr) Error() string { return e.msg }

func errf(code int, f string, a ...any) error { return apiErr{code, fmt.Sprintf(f, a...)} }

func (f *fakeGCP) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	path := strings.TrimPrefix(r.URL.Path, "/")
	if r.Method != http.MethodGet {
		f.calls = append(f.calls, r.Method+" "+path)
	}
	svc, rest, _ := strings.Cut(path, "/")
	var out any
	var err error
	switch svc {
	case "compute":
		out, err = f.compute(r, rest, body)
	case "iam":
		out, err = f.iam(r, strings.TrimPrefix(rest, "v1/"), body)
	case "crm":
		out, err = f.crm(r, strings.TrimPrefix(rest, "v1/"), body)
	case "secretmanager":
		out, err = f.secretmanager(r, strings.TrimPrefix(rest, "v1/"), body)
	case "monitoring":
		out, err = f.monitoring(r, strings.TrimPrefix(rest, "v3/"), body)
	default:
		err = errf(404, "no service %s", svc)
	}
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		code := 500
		var ae apiErr
		if e, ok := err.(apiErr); ok {
			ae = e
			code = ae.code
		}
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": err.Error()}})
		return
	}
	if out == nil {
		out = map[string]any{}
	}
	json.NewEncoder(w).Encode(out)
}

func decode[T any](t *testing.T, body []byte) *T {
	v := new(T)
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("bad request body: %v: %s", err, body)
	}
	return v
}

func (f *fakeGCP) newFP() string { f.fp++; return fmt.Sprintf("fp%d", f.fp) }

func done(zone string) *compute.Operation {
	return &compute.Operation{Name: "op", Status: "DONE", Zone: zone}
}

func last(s string) string { return s[strings.LastIndex(s, "/")+1:] }

// ---------------- compute ----------------

var (
	reZonal    = regexp.MustCompile(`^projects/[^/]+/zones/([^/]+)/(instances|disks)(?:/([^/]+))?(?:/([A-Za-z]+))?$`)
	reRegional = regexp.MustCompile(`^projects/[^/]+/regions/([^/]+)/resourcePolicies(?:/([^/]+))?$`)
	reGlobal   = regexp.MustCompile(`^projects/[^/]+/global/(firewalls|snapshots)(?:/([^/]+))?$`)
)

func (f *fakeGCP) compute(r *http.Request, p string, body []byte) (any, error) {
	if strings.HasSuffix(p, "/aggregated/instances") {
		items := map[string]compute.InstancesScopedList{}
		for _, in := range f.instances {
			z := "zones/" + last(in.Zone)
			l := items[z]
			l.Instances = append(l.Instances, in)
			items[z] = l
		}
		return &compute.InstanceAggregatedList{Items: items}, nil
	}
	if m := reRegional.FindStringSubmatch(p); m != nil {
		return f.resourcePolicies(r, m[1], m[2], body)
	}
	if m := reGlobal.FindStringSubmatch(p); m != nil {
		if m[1] == "firewalls" {
			return f.firewallsAPI(r, m[2], body)
		}
		return f.snapshotsAPI(r, m[2])
	}
	m := reZonal.FindStringSubmatch(p)
	if m == nil {
		return nil, errf(404, "unknown compute path %s", p)
	}
	if m[2] == "disks" {
		return f.disksAPI(r, m[1], m[3], m[4], body)
	}
	return f.instancesAPI(r, m[1], m[3], m[4], body)
}

func (f *fakeGCP) instancesAPI(r *http.Request, zone, name, verb string, body []byte) (any, error) {
	if name == "" && r.Method == http.MethodPost {
		in := decode[compute.Instance](f.t, body)
		if f.instances[in.Name] != nil {
			return nil, errf(409, "instance %s exists", in.Name)
		}
		sa := in.ServiceAccounts[0].Email
		if f.sas[sa] == nil {
			return nil, errf(400, "Invalid value for field 'resource.serviceAccounts[0].email': %s does not exist", sa)
		}
		f.nextID++
		in.Id, in.Status, in.Zone = f.nextID, "RUNNING", "zones/"+zone
		in.Metadata.Fingerprint, in.LabelFingerprint = f.newFP(), f.newFP()
		for i, nic := range in.NetworkInterfaces {
			nic.Name = fmt.Sprintf("nic%d", i)
		}
		for _, d := range in.Disks {
			if d.Boot {
				f.disks[in.Name] = &compute.Disk{Name: in.Name, SizeGb: d.InitializeParams.DiskSizeGb, Status: "READY", Users: []string{in.Name}}
				d.Source, d.DiskSizeGb = "projects/p/zones/"+zone+"/disks/"+in.Name, d.InitializeParams.DiskSizeGb
				d.InitializeParams = nil
				continue
			}
			cd := f.disks[last(d.Source)]
			if cd == nil {
				return nil, errf(400, "disk %s not found", d.Source)
			}
			if len(cd.Users) > 0 {
				return nil, errf(400, "disk %s in use", cd.Name)
			}
			cd.Users = []string{in.Name}
			d.DiskSizeGb = cd.SizeGb
		}
		f.instances[in.Name] = in
		return done(zone), nil
	}
	in := f.instances[name]
	if in == nil {
		return nil, errf(404, "instance %s not found", name)
	}
	needStatus := func(want ...string) error {
		if !slices.Contains(want, in.Status) {
			return errf(400, "%s: instance is %s, needs %v", verb, in.Status, want)
		}
		return nil
	}
	switch {
	case r.Method == http.MethodGet && verb == "":
		return in, nil
	case r.Method == http.MethodGet && verb == "getIamPolicy":
		if p := f.instIAM[name]; p != nil {
			return p, nil
		}
		return &compute.Policy{Etag: "i0"}, nil
	case r.Method == http.MethodDelete:
		for _, d := range in.Disks {
			if d.AutoDelete {
				delete(f.disks, last(d.Source))
			} else if cd := f.disks[last(d.Source)]; cd != nil {
				cd.Users = nil
			}
		}
		delete(f.instances, name)
		delete(f.instIAM, name)
		return done(zone), nil
	}
	switch verb {
	case "setIamPolicy":
		req := decode[compute.ZoneSetPolicyRequest](f.t, body)
		f.instIAM[name] = req.Policy
		return req.Policy, nil
	case "suspend":
		if err := needStatus("RUNNING"); err != nil {
			return nil, err
		}
		in.Status = "SUSPENDED"
	case "resume":
		if err := needStatus("SUSPENDED"); err != nil {
			return nil, err
		}
		in.Status = "RUNNING"
	case "stop":
		if err := needStatus("RUNNING"); err != nil {
			return nil, err
		}
		in.Status = "TERMINATED"
	case "start":
		if err := needStatus("TERMINATED"); err != nil {
			return nil, err
		}
		in.Status = "RUNNING"
	case "setMachineType":
		if err := needStatus("TERMINATED"); err != nil {
			return nil, err
		}
		in.MachineType = decode[compute.InstancesSetMachineTypeRequest](f.t, body).MachineType
	case "setServiceAccount":
		if err := needStatus("TERMINATED"); err != nil {
			return nil, err
		}
		req := decode[compute.InstancesSetServiceAccountRequest](f.t, body)
		in.ServiceAccounts = []*compute.ServiceAccount{{Email: req.Email, Scopes: req.Scopes}}
	case "updateShieldedInstanceConfig":
		if err := needStatus("TERMINATED"); err != nil {
			return nil, err
		}
		in.ShieldedInstanceConfig = decode[compute.ShieldedInstanceConfig](f.t, body)
	case "setMetadata":
		if err := needStatus("RUNNING", "TERMINATED"); err != nil {
			return nil, err
		}
		md := decode[compute.Metadata](f.t, body)
		if md.Fingerprint != in.Metadata.Fingerprint {
			return nil, errf(412, "metadata fingerprint mismatch")
		}
		md.Fingerprint = f.newFP()
		in.Metadata = md
	case "setLabels":
		req := decode[compute.InstancesSetLabelsRequest](f.t, body)
		if req.LabelFingerprint != in.LabelFingerprint {
			return nil, errf(412, "label fingerprint mismatch")
		}
		in.Labels, in.LabelFingerprint = req.Labels, f.newFP()
	case "addAccessConfig":
		in.NetworkInterfaces[0].AccessConfigs = append(in.NetworkInterfaces[0].AccessConfigs, decode[compute.AccessConfig](f.t, body))
	case "deleteAccessConfig":
		in.NetworkInterfaces[0].AccessConfigs = nil
	case "attachDisk":
		ad := decode[compute.AttachedDisk](f.t, body)
		cd := f.disks[last(ad.Source)]
		if cd == nil || len(cd.Users) > 0 {
			return nil, errf(400, "cannot attach %s", ad.Source)
		}
		cd.Users = []string{name}
		in.Disks = append(in.Disks, ad)
	default:
		return nil, errf(404, "unknown instance verb %s", verb)
	}
	return done(zone), nil
}

func (f *fakeGCP) disksAPI(r *http.Request, zone, name, verb string, body []byte) (any, error) {
	if name == "" && r.Method == http.MethodPost {
		d := decode[compute.Disk](f.t, body)
		if f.disks[d.Name] != nil {
			return nil, errf(409, "disk exists")
		}
		d.Status, d.LabelFingerprint = "READY", f.newFP()
		f.disks[d.Name] = d
		return done(zone), nil
	}
	d := f.disks[name]
	if d == nil {
		return nil, errf(404, "disk %s not found", name)
	}
	switch {
	case r.Method == http.MethodGet:
		return d, nil
	case r.Method == http.MethodDelete:
		if len(d.Users) > 0 {
			return nil, errf(400, "resourceInUseByAnotherResource: disk %s is attached", name)
		}
		delete(f.disks, name)
		return done(zone), nil
	}
	switch verb {
	case "resize":
		n := decode[compute.DisksResizeRequest](f.t, body).SizeGb
		if n <= d.SizeGb {
			return nil, errf(400, "disks only grow")
		}
		d.SizeGb = n
		for _, in := range f.instances {
			for _, ad := range in.Disks {
				if last(ad.Source) == name {
					ad.DiskSizeGb = n
				}
			}
		}
	case "setLabels":
		req := decode[compute.ZoneSetLabelsRequest](f.t, body)
		if req.LabelFingerprint != d.LabelFingerprint {
			return nil, errf(412, "label fingerprint mismatch")
		}
		d.Labels, d.LabelFingerprint = req.Labels, f.newFP()
	case "addResourcePolicies":
		for _, p := range decode[compute.DisksAddResourcePoliciesRequest](f.t, body).ResourcePolicies {
			if f.policies[last(p)] == nil {
				return nil, errf(400, "policy %s not found", p)
			}
			d.ResourcePolicies = append(d.ResourcePolicies, p)
		}
	case "removeResourcePolicies":
		rm := decode[compute.DisksRemoveResourcePoliciesRequest](f.t, body).ResourcePolicies
		d.ResourcePolicies = slices.DeleteFunc(d.ResourcePolicies, func(p string) bool {
			return slices.ContainsFunc(rm, func(x string) bool { return last(x) == last(p) })
		})
	default:
		return nil, errf(404, "unknown disk verb %s", verb)
	}
	return done(zone), nil
}

func (f *fakeGCP) resourcePolicies(r *http.Request, region, name string, body []byte) (any, error) {
	if name == "" && r.Method == http.MethodPost {
		rp := decode[compute.ResourcePolicy](f.t, body)
		rp.Status, rp.SelfLink = "READY", "projects/p/regions/"+region+"/resourcePolicies/"+rp.Name
		f.policies[rp.Name] = rp
		return done(""), nil
	}
	rp := f.policies[name]
	if rp == nil {
		return nil, errf(404, "policy %s not found", name)
	}
	switch r.Method {
	case http.MethodGet:
		return rp, nil
	case http.MethodPatch:
		rp.SnapshotSchedulePolicy = decode[compute.ResourcePolicy](f.t, body).SnapshotSchedulePolicy
	case http.MethodDelete:
		for _, d := range f.disks {
			if slices.ContainsFunc(d.ResourcePolicies, func(p string) bool { return last(p) == name }) {
				return nil, errf(400, "policy %s is in use by %s", name, d.Name)
			}
		}
		delete(f.policies, name)
	}
	return done(""), nil
}

func (f *fakeGCP) firewallsAPI(r *http.Request, name string, body []byte) (any, error) {
	if name == "" && r.Method == http.MethodPost {
		fw := decode[compute.Firewall](f.t, body)
		if f.firewalls[fw.Name] != nil {
			return nil, errf(409, "firewall exists")
		}
		f.firewalls[fw.Name] = fw
		return done(""), nil
	}
	if name == "" && r.Method == http.MethodGet {
		out := &compute.FirewallList{}
		for _, fw := range f.firewalls {
			out.Items = append(out.Items, fw)
		}
		return out, nil
	}
	fw := f.firewalls[name]
	if fw == nil {
		return nil, errf(404, "firewall %s not found", name)
	}
	switch r.Method {
	case http.MethodGet:
		return fw, nil
	case http.MethodPut:
		f.firewalls[name] = decode[compute.Firewall](f.t, body)
	case http.MethodDelete:
		delete(f.firewalls, name)
	}
	return done(""), nil
}

var reLabelFilter = regexp.MustCompile(`labels\.devbox\s*=\s*"?([^"]+)"?`)

func (f *fakeGCP) snapshotsAPI(r *http.Request, name string) (any, error) {
	if name == "" {
		want := ""
		if m := reLabelFilter.FindStringSubmatch(r.URL.Query().Get("filter")); m != nil {
			want = m[1]
		}
		l := &compute.SnapshotList{}
		for _, s := range f.snapshots {
			if want == "" || s.Labels["devbox"] == want {
				l.Items = append(l.Items, s)
			}
		}
		return l, nil
	}
	if f.snapshots[name] == nil {
		return nil, errf(404, "snapshot not found")
	}
	if r.Method == http.MethodDelete {
		delete(f.snapshots, name)
		return done(""), nil
	}
	return f.snapshots[name], nil
}

// ---------------- iam ----------------

func (f *fakeGCP) iam(r *http.Request, p string, body []byte) (any, error) {
	parts := strings.Split(p, "/")
	if len(parts) < 3 {
		return nil, errf(404, "bad iam path %s", p)
	}
	switch parts[2] {
	case "serviceAccounts":
		if len(parts) == 3 {
			req := decode[iam.CreateServiceAccountRequest](f.t, body)
			email := req.AccountId + "@" + parts[1] + ".iam.gserviceaccount.com"
			if f.sas[email] != nil {
				return nil, errf(409, "exists")
			}
			sa := req.ServiceAccount
			sa.Email = email
			f.sas[email] = sa
			return sa, nil
		}
		email, verb, _ := strings.Cut(parts[3], ":")
		sa := f.sas[email]
		if sa == nil {
			return nil, errf(404, "service account %s not found", email)
		}
		switch {
		case verb == "enable":
			sa.Disabled = false
		case r.Method == http.MethodDelete:
			delete(f.sas, email)
		}
		return sa, nil
	case "roles":
		if len(parts) == 3 {
			req := decode[iam.CreateRoleRequest](f.t, body)
			if f.roles[req.RoleId] != nil {
				return nil, errf(400, "role id %s is in use or was recently deleted", req.RoleId)
			}
			role := req.Role
			role.Name, role.Etag = "projects/"+parts[1]+"/roles/"+req.RoleId, "r0"
			f.roles[req.RoleId] = role
			return role, nil
		}
		id, verb, _ := strings.Cut(parts[3], ":")
		role := f.roles[id]
		if role == nil {
			return nil, errf(404, "role %s not found", id)
		}
		switch {
		case verb == "undelete":
			role.Deleted = false
		case r.Method == http.MethodDelete:
			role.Deleted = true
		case r.Method == http.MethodPatch:
			role.IncludedPermissions = decode[iam.Role](f.t, body).IncludedPermissions
		}
		return role, nil
	}
	return nil, errf(404, "bad iam path %s", p)
}

// ---------------- resource manager ----------------

func (f *fakeGCP) crm(r *http.Request, p string, body []byte) (any, error) {
	proj, verb, _ := strings.Cut(strings.TrimPrefix(p, "projects/"), ":")
	switch verb {
	case "":
		return &crm.Project{ProjectId: proj, ProjectNumber: fakeProjectNumber, LifecycleState: "ACTIVE"}, nil
	case "getIamPolicy":
		return f.projPolicy, nil
	case "setIamPolicy":
		pol := decode[crm.SetIamPolicyRequest](f.t, body).Policy
		if pol.Etag != f.projPolicy.Etag {
			return nil, errf(409, "etag mismatch")
		}
		for _, b := range pol.Bindings {
			if b.Condition != nil && pol.Version != 3 {
				return nil, errf(400, "conditions need policy version 3")
			}
			for _, m := range b.Members {
				if email, ok := strings.CutPrefix(m, "serviceAccount:"); ok && f.sas[email] == nil {
					return nil, errf(400, "Service account %s does not exist", email)
				}
			}
		}
		pol.Etag = f.newFP()
		f.projPolicy = pol
		return pol, nil
	}
	return nil, errf(404, "bad crm path %s", p)
}

// ---------------- secret manager ----------------

func (f *fakeGCP) addSecret(id, box string, versions int) {
	fs := &fakeSecret{s: &secretmanager.Secret{Name: "projects/p/secrets/" + id, Labels: map[string]string{"devbox": box}}}
	for i := 1; i <= versions; i++ {
		fs.versions = append(fs.versions, &secretmanager.SecretVersion{Name: fmt.Sprintf("%s/versions/%d", fs.s.Name, i), State: "ENABLED", CreateTime: fmt.Sprintf("2026-01-0%dT00:00:00Z", i)})
	}
	f.secrets[id] = fs
}

func (f *fakeGCP) secretmanager(r *http.Request, p string, body []byte) (any, error) {
	parts := strings.Split(p, "/") // projects/P/secrets[/ID[:verb]][/versions[/V[:verb]]]
	if len(parts) == 3 {
		if r.Method == http.MethodPost {
			id := r.URL.Query().Get("secretId")
			if f.secrets[id] != nil {
				return nil, errf(409, "secret exists")
			}
			s := decode[secretmanager.Secret](f.t, body)
			s.Name = "projects/p/secrets/" + id
			f.secrets[id] = &fakeSecret{s: s}
			return s, nil
		}
		want := ""
		if m := regexp.MustCompile(`labels\.devbox=(\S+)`).FindStringSubmatch(r.URL.Query().Get("filter")); m != nil {
			want = m[1]
		}
		out := &secretmanager.ListSecretsResponse{}
		for _, s := range f.secrets {
			if want == "" || s.s.Labels["devbox"] == want {
				out.Secrets = append(out.Secrets, s.s)
			}
		}
		return out, nil
	}
	id, verb, _ := strings.Cut(parts[3], ":")
	s := f.secrets[id]
	if s == nil {
		return nil, errf(404, "secret %s not found", id)
	}
	if len(parts) == 4 {
		switch {
		case verb == "getIamPolicy":
			if s.policy == nil {
				return &secretmanager.Policy{Etag: "s0"}, nil
			}
			return s.policy, nil
		case verb == "setIamPolicy":
			pol := decode[secretmanager.SetIamPolicyRequest](f.t, body).Policy
			if s.policy != nil && pol.Etag != s.policy.Etag {
				return nil, errf(409, "etag mismatch")
			}
			for _, b := range pol.Bindings {
				for _, m := range b.Members {
					if email, ok := strings.CutPrefix(m, "serviceAccount:"); ok && f.sas[email] == nil {
						return nil, errf(400, "Service account %s does not exist", email)
					}
				}
			}
			pol.Etag = f.newFP()
			s.policy = pol
			return pol, nil
		case verb == "addVersion":
			v := &secretmanager.SecretVersion{Name: fmt.Sprintf("%s/versions/%d", s.s.Name, len(s.versions)+1), State: "ENABLED",
				CreateTime: fmt.Sprintf("2026-02-%02dT00:00:00Z", len(s.versions)+1)}
			s.versions = append(s.versions, v)
			if s.data == nil {
				s.data = map[string]string{}
			}
			s.data[v.Name] = decode[secretmanager.AddSecretVersionRequest](f.t, body).Payload.Data
			return v, nil
		case r.Method == http.MethodDelete:
			delete(f.secrets, id)
			return nil, nil
		}
		return s.s, nil
	}
	if len(parts) == 5 { // list versions
		out := &secretmanager.ListSecretVersionsResponse{}
		for _, v := range s.versions {
			if v.State == "ENABLED" {
				out.Versions = append(out.Versions, v)
			}
		}
		return out, nil
	}
	vid, vverb, _ := strings.Cut(parts[5], ":")
	if vid == "latest" {
		for i := len(s.versions) - 1; i >= 0; i-- {
			if v := s.versions[i]; v.State == "ENABLED" {
				if vverb == "access" {
					return &secretmanager.AccessSecretVersionResponse{Name: v.Name, Payload: &secretmanager.SecretPayload{Data: s.data[v.Name]}}, nil
				}
				return v, nil
			}
		}
		return nil, errf(404, "no enabled version")
	}
	for _, v := range s.versions {
		if last(v.Name) == vid {
			if vverb == "disable" {
				v.State = "DISABLED"
			}
			return v, nil
		}
	}
	return nil, errf(404, "version not found")
}

// ---------------- monitoring ----------------

func (f *fakeGCP) monitoring(r *http.Request, p string, body []byte) (any, error) {
	parts := strings.Split(p, "/") // projects/P/{notificationChannels|alertPolicies}[/ID]
	if len(parts) < 3 {
		return nil, errf(404, "bad monitoring path")
	}
	kind := parts[2]
	if len(parts) == 3 {
		if r.Method == http.MethodGet {
			if kind == "timeSeries" {
				ts := &monitoring.TimeSeries{}
				for _, v := range f.uptimeSecs {
					ts.Points = append(ts.Points, &monitoring.Point{Value: &monitoring.TypedValue{DoubleValue: &v}})
				}
				return &monitoring.ListTimeSeriesResponse{TimeSeries: []*monitoring.TimeSeries{ts}}, nil
			}
			if kind == "notificationChannels" {
				out := &monitoring.ListNotificationChannelsResponse{}
				for _, k := range sortedKeys(f.channels) {
					out.NotificationChannels = append(out.NotificationChannels, f.channels[k])
				}
				return out, nil
			}
			out := &monitoring.ListAlertPoliciesResponse{}
			for _, k := range sortedKeys(f.alerts) {
				out.AlertPolicies = append(out.AlertPolicies, f.alerts[k])
			}
			return out, nil
		}
		f.nextID++
		name := fmt.Sprintf("projects/%s/%s/%d", parts[1], kind, f.nextID)
		if kind == "notificationChannels" {
			c := decode[monitoring.NotificationChannel](f.t, body)
			c.Name = name
			f.channels[name] = c
			return c, nil
		}
		a := decode[monitoring.AlertPolicy](f.t, body)
		for _, ch := range a.NotificationChannels {
			if f.channels[ch] == nil {
				return nil, errf(400, "channel %s does not exist", ch)
			}
		}
		a.Name = name
		f.alerts[name] = a
		return a, nil
	}
	name := strings.Join(parts[:4], "/")
	switch kind {
	case "notificationChannels":
		c := f.channels[name]
		if c == nil {
			return nil, errf(404, "channel not found")
		}
		switch r.Method {
		case http.MethodPatch:
			c.Labels = decode[monitoring.NotificationChannel](f.t, body).Labels
		case http.MethodDelete:
			delete(f.channels, name)
		}
		return c, nil
	default:
		a := f.alerts[name]
		if a == nil {
			return nil, errf(404, "policy not found")
		}
		switch r.Method {
		case http.MethodPatch:
			a = decode[monitoring.AlertPolicy](f.t, body)
			a.Name = name
			f.alerts[name] = a
		case http.MethodDelete:
			delete(f.alerts, name)
		}
		return a, nil
	}
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

// callsSince returns mutating calls made after mark, with verbs only.
func (f *fakeGCP) callsSince(mark int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls[mark:])
}

func (f *fakeGCP) mark() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}
