# devbox

A Claude Code workstation in your own GCP project, reachable over your tailnet, built and torn down by one static binary. No Terraform, no state file.

## What it does

`devbox` makes a GCP project match `devbox.yaml`. It finds everything it created by the label `devbox=<name>` or the name prefix `<name>-`, so there is no state to keep.

**The box**
- An Ubuntu 24.04 VM (`e2-standard-2` by default) on an existing VPC subnet.
- Reached over Tailscale SSH or mosh. Two fallbacks: SSH through IAP, and an optional break-glass firewall rule for one address.
- You log in with your Google account through OS Login. OS Login admins get sudo.
- It suspends itself after 60 idle minutes: no attached tmux client, SSH session, mosh session or `claude` process. Suspend keeps memory, so tmux and Claude Code sessions survive. Resuming takes about 30 seconds.
- Email alerts: CPU above 80% for 5 minutes, CPU below 5% for 6 hours (idle suspend has failed), `/srv/code` above 85%.

**`/srv/code`, the disk that survives**
- A separate 50 GB disk, snapshotted daily at 03:00 UTC and kept 14 days.
- Holds your projects, your Claude Code login, your gcloud configs, and the box's Tailscale identity and SSH host keys.
- A rebuilt VM comes back as the same tailnet device, with the same host key and everything on this disk.

**Identity**
- A dedicated ed25519 key, separate from your personal keys, lives in Secret Manager.
- At boot it is loaded into a system ssh-agent and never written to the box's disk.
- It authenticates to GitHub and signs commits (SSH signing; GitHub shows them as Verified).
- Only admins can use the agent; each connection is checked against OS Login.

**Claude Code**
- Installed from Anthropic's signed apt repository.
- Managed settings and a hook deny agents `git commit`, `git push`, `terraform apply/destroy` and `devbox apply/destroy`. This is policy, not a security boundary; branch protection on GitHub is the real backstop.
- A status line in every session: directory and branch, the gcloud identity and project the session was given, whether that identity has Application Default Credentials, how full `/srv/code` is, context and rate-limit meters, session cost, and the model.

**Projects**
- Each project is a clone under `/srv/code` with its own environment file and tmux session.
- Secrets are referenced as `@secret:<name>`, read from Secret Manager with the box's own identity, and held only in the tmux session's memory.

**What survives what**

| Event | Kept | Lost |
|---|---|---|
| `down` / idle suspend, then `up` | everything, including running sessions | nothing |
| `reprovision` | everything | nothing |
| `apply` that restarts (machine type) | both disks | memory: tmux and Claude sessions |
| `apply` that replaces (cloud-init or network change) | `/srv/code`, tailnet identity, host keys, Claude login | home directories, sessions |
| `destroy` | code disk, its snapshots, `devbox-*` secrets | the VM and everything else |
| `destroy --all` | nothing | everything it created |

`destroy` never touches the project, the VPC or which APIs are enabled.

**Cost:** about $11 a month while suspended (disks plus suspended memory). Running adds the region's `e2-standard-2` on-demand price and an ephemeral IP. `./devbox status` prints an estimate.

## How to use it

### Every day

```bash
./devbox up                                   # create what is missing; resume if suspended
mosh slava@devbox -- tmux new -A -s main      # or: ssh slava@devbox
./devbox down                                 # suspend; sessions survive
./devbox status                               # what exists, its state, what it costs
```

If `ssh` hangs or `gcloud compute ssh` fails with `4003: 'failed to connect to backend'`, the box has suspended itself: run `./devbox up`.

If `devbox` says your Google login expired, run `gcloud auth application-default login`. If that keeps happening, a Workspace admin can relax **Security → Access and data control → Google Cloud session control** at admin.google.com.

To see what the idle check last counted, run `cat /run/devbox-idle.status` on the box.

### Projects and secrets

From the laptop:

```bash
./devbox project add git@github.com:acme/api.git --gcloud-config client-a --gcp-project acme-prod --region europe-west2
./devbox project set acme-api --gcloud-config client-b --recreate   # switch identity, restart the session
./devbox project set acme-api --unset gcloud-config                 # back to /srv/code/.gcloud/default
./devbox project secret acme-api OPENAI_API_KEY             # prompts; input hidden
pbpaste | ./devbox project secret acme-api DATABASE_URL     # or from stdin
./devbox project secret acme-api OPENAI_API_KEY --delete
```

- **`project add`** clones into `/srv/code/acme-api/` and writes `.devbox-env`. It copies the non-secret variables into `.claude/settings.local.json`, for agents, and creates a tmux session named `acme-api`. `--env KEY=VALUE` adds any others.
- **`project set`** changes an existing project's variables with the same flags, and removes them with `--unset KEY` (or `--unset gcloud-config|gcp-project|region`). The running session keeps the old values unless you pass `--recreate`, which restarts it and kills whatever is running in it. Restart Claude Code sessions in the project too.
- **`project secret`** stores `devbox-acme-api-OPENAI_API_KEY` in Secret Manager and adds `OPENAI_API_KEY=@secret:devbox-acme-api-OPENAI_API_KEY` to `.devbox-env`. Running it again rotates the value and disables older versions.

Sessions read secrets when they are created. After adding or rotating one, recreate the session on the box:

```bash
devbox-sessions --recreate acme-api
```

Where values live:

| Place | Holds secret values? |
|---|---|
| Secret Manager | yes, the only copy |
| `.devbox-env` | no, only the `@secret:` reference |
| the tmux session's environment | yes, in memory |
| `.claude/settings.local.json` | never |

`--gcloud-config client-a` points the project at `/srv/code/.gcloud/client-a`. Log that identity in once, on the box:

```bash
export CLOUDSDK_CONFIG=/srv/code/.gcloud/client-a
gcloud auth login --no-launch-browser
gcloud auth application-default login --no-launch-browser
```

To remove a project, run `tmux kill-session -t acme-api && rm -rf /srv/code/acme-api` on the box, then delete its secrets with `--delete`.

### Files

| Job | Use |
|---|---|
| Browse or drop files from Finder | Taildrive share of `/srv/code/_out` |
| Push or pull a directory | rsync over SSH |
| Grab one big file | `tailscale serve` on `/srv/code/_out` |

Only `_out` is ever shared, never `/srv/code`, which holds every project.

**Finder.** In Finder press ⌘K, connect to `http://100.100.100.100:8080` (as Guest if asked), then open your tailnet's folder, then `devbox`, then `out`.

**rsync.** Swap the last two arguments to pull; run it again to resume:

```bash
rsync -aHAX --info=progress2 --partial-dir=.rsync-partial -z --timeout=120 \
      -e 'ssh -o ControlMaster=auto -o ControlPath=~/.ssh/cm-%C -o ControlPersist=10m' \
      ./dataset/ slava@devbox:/srv/code/acme-api/dataset/
```

**One file.** On the box:

```bash
tailscale serve --bg /srv/code/_out
tailscale status --json | jq -r .Self.DNSName      # e.g. devbox.tail1234.ts.net.
```

On the laptop, using that name without the trailing dot. Run it again to resume:

```bash
curl -fC - -O https://devbox.tail1234.ts.net/report.pdf
```

Stop serving with `tailscale serve --https=443 off`.

### Changing things

| Change | How | Cost |
|---|---|---|
| Anything under `/srv/code`, your dotfiles | edit on the box | none |
| `provision.sh`, files under `internal/assets/files/box/`, or a reprovision-tier key | `make build && ./devbox reprovision` | about 5 s, nothing lost |
| `machine_type` | `./devbox apply` | restart; sessions lost |
| `cloud-init.yaml` (packages, systemd units, sshd), network or subnet | `./devbox apply` | replacement; sessions and home directories lost |

- Run `./devbox apply --dry-run` first; it prints the plan and changes nothing.
- `apply` asks before a restart or a replacement, and always resumes a suspended box before changing it.
- If the tailnet is down, use `./devbox reprovision --iap`.
- Every key's tier is in [docs/config.md](docs/config.md).

Try replacement-tier changes on a throwaway box first:

```bash
./devbox up --name devbox-test --code-disk-gb 20
./devbox destroy --name devbox-test --all
```

### Tearing down and restoring

```bash
./devbox destroy          # keeps the code disk, its snapshots and the devbox-* secrets
./devbox destroy --all    # deletes those too; asks you to type the disk name
./devbox status           # every row should be absent, or present only for what you kept
```

To restore `/srv/code` from a snapshot:

```bash
ZONE=europe-west4-a
gcloud compute snapshots list --filter="labels.devbox=devbox" --sort-by=~creationTimestamp
SNAP=PASTE-A-NAME-FROM-THE-LIST
./devbox destroy
gcloud compute snapshots create devbox-code-before-restore --source-disk=devbox-code --source-disk-zone=$ZONE
gcloud compute disks delete devbox-code --zone=$ZONE
gcloud compute disks create devbox-code --zone=$ZONE --type=pd-balanced --source-snapshot="$SNAP" --labels=devbox=devbox
./devbox up
```

## How to set it up

You need:
- a GCP project where you hold `roles/owner`, plus `roles/compute.osAdminLogin`, which owner includes
- a Tailscale account
- a GitHub account
- on the laptop: Go, `gcloud`, `tailscale`, `gh` and `mosh`

### 1. Build and log in

```bash
make setup     # builds ./devbox and creates devbox.yaml from devbox.example.yaml
make auth      # gcloud login, plus the Application Default Credentials the binary uses
```

In `devbox.yaml`, set `project_id`, `git.name`, `git.email` and `alerts.email`. Every key is described in [docs/config.md](docs/config.md).

### 2. Network

`devbox` never creates or deletes networks. Give it a VPC and a subnet in the zone's region. It adds only its own firewall rules:

```bash
P=YOUR-PROJECT
gcloud compute networks create devbox --project $P --subnet-mode=custom
gcloud compute networks subnets create devbox-europe-west4 --project $P --network devbox \
  --region europe-west4 --range 10.10.0.0/24 --enable-private-ip-google-access
```

Then set `network.name: devbox` and `network.subnet: devbox-europe-west4` in `devbox.yaml`.

### 3. Tailscale

1. **Access controls.** Switch to the **JSON editor** and add these keys inside the outer `{ … }`, merging with any keys that already exist. `"users"` is your login name on the box (see step 5); `autogroup:nonroot` works until you know it.
   ```
   "tagOwners": { "tag:devbox": ["autogroup:admin"] },
   "ssh": [
     { "action": "accept", "src": ["autogroup:member"], "dst": ["tag:devbox"], "users": ["slava", "root"] }
   ],
   "nodeAttrs": [
     { "target": ["tag:devbox"],       "attr": ["drive:share"] },
     { "target": ["autogroup:member"], "attr": ["drive:access"] }
   ],
   ```
   Add this entry to the existing `"grants"` list. It allows the Finder mount; you can leave it and the `nodeAttrs` out.
   ```
   { "src": ["autogroup:member"], "dst": ["tag:devbox"],
     "app": { "tailscale.com/cap/drive": [{ "shares": ["out"], "access": "rw" }] } }
   ```
2. **OAuth client.** Go to Settings → **Trust credentials** → Credential → OAuth. Choose scope **Auth Keys (write)** and tag `tag:devbox`, then Generate credential. Store the secret, which is shown only once:
   ```bash
   printf '%s' 'PASTE-THE-SECRET' | gcloud secrets create devbox-tailscale-oauth --data-file=- --labels=devbox=devbox
   ```

### 4. Machine key

```bash
ssh-keygen -t ed25519 -N '' -C 'devbox' -f devbox_ed25519
gcloud secrets create devbox-ssh-key --data-file=devbox_ed25519 --labels=devbox=devbox
gh auth refresh -s admin:public_key,admin:ssh_signing_key
gh ssh-key add devbox_ed25519.pub --type authentication --title devbox
gh ssh-key add devbox_ed25519.pub --type signing --title devbox
gcloud secrets versions access latest --secret devbox-ssh-key >/dev/null && rm -P devbox_ed25519 devbox_ed25519.pub
```

Both `gh ssh-key add` lines are needed. With only the first, pushes work but commits show Unverified. The last line deletes the local copy only after the secret is confirmed to exist.

### 5. Your login name (optional)

OS Login derives your name from your email, for example `slava_coolconsulting_lol`. To use a short name, a Google Workspace super admin can set it with [users.patch](https://developers.google.com/admin-sdk/directory/reference/rest/v1/users/patch). Use **Try it**, with `userKey` set to your email, and this body:

```json
{ "posixAccounts": [{ "username": "slava", "uid": "YOUR_CURRENT_UID", "gid": "YOUR_CURRENT_UID",
    "homeDirectory": "/home/slava", "shell": "/bin/bash", "operatingSystemType": "linux", "primary": true }] }
```

Find the current uid with `gcloud compute os-login describe-profile`. Keep the same uid, so files on `/srv/code` stay yours. The new name applies to every OS Login VM in the organization.

### 6. Preflight and up

```bash
./devbox preflight --fix     # --fix enables missing APIs; every other line says how to fix it
./devbox up
```

Provisioning takes about 3 minutes after `up` returns.

### 7. First login, over IAP

On a new code disk, the first login must go through IAP. That login is what grants your account sudo and enrolls it on `/srv/code`. After it, Tailscale SSH alone is enough, including after replacements.

```bash
gcloud compute ssh devbox --zone europe-west4-a --tunnel-through-iap
```

Then, from the laptop:

```bash
ssh slava@devbox 'ssh-add -L; ssh -T git@github.com'
```

You should see the `devbox` key, then "Hi <you>! You've successfully authenticated".

### 8. mosh on macOS

The macOS firewall silently drops replies to the unsigned Homebrew `mosh-client`. Allow it, and again after every `brew upgrade mosh`, because the path includes the version:

```bash
mc=$(realpath /opt/homebrew/bin/mosh-client)
sudo /usr/libexec/ApplicationFirewall/socketfilterfw --add "$mc"
sudo /usr/libexec/ApplicationFirewall/socketfilterfw --unblockapp "$mc"
```

### 9. Claude Code

On the box, run `claude` and log in with your subscription:
1. Press `c` to copy the URL and open it on the laptop.
2. If the browser shows a code, paste it at the `Paste code here` prompt.

The login is stored on `/srv/code`, so replacements keep it. Claude Code warns three days before the login expires; renew it then.

Managed settings, the guard hook and the status line script live under `internal/assets/files/box/` and are written by `reprovision`, so changing Claude Code policy never replaces the box. In a project session it shows the gcloud config, project and region from `.devbox-env`, and `no ADC` in red until that config has run `gcloud auth application-default login`.

### 10. Finder share (optional)

On the box:

```bash
sudo tailscale set --operator=$USER
tailscale drive share out /srv/code/_out
```

Both settings live with the box's Tailscale identity on `/srv/code` and survive replacements.
