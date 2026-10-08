# 01 · First run

**From nothing to a shell on the box?** Rationale lives in [plan.md](plan.md); this page only has steps.

## 1. Laptop

```bash
brew install --cask google-cloud-sdk && brew install tailscale gh   # or your platform's equivalents
make auth          # gcloud login, plus the Application Default Credentials the binary uses
make setup         # builds ./devbox and creates devbox.yaml from the example
```

In `devbox.yaml`, set `project_id`, `git.name`, `git.email` and `alerts.email`.

## 2. Preflight

```bash
./devbox preflight --fix
```

`--fix` only enables APIs. For anything else marked `FAIL` or `MISSING`, the line itself tells you the fix. The common ones:

| Line | Fix |
|---|---|
| `permissions … missing` | Ask for `roles/owner`, or editor + `roles/resourcemanager.projectIamAdmin` + `roles/iam.roleAdmin` |
| `os admin login FAIL` | `gcloud projects add-iam-policy-binding PROJECT --member=user:YOU --role=roles/compute.osAdminLogin` |
| `network … no subnet` | `zone` is outside the subnet's region, or the names in `network:` are wrong |
| `org policies compute.vmExternalIpAccess` | Set `external_ip: false` and make sure the subnet has Cloud NAT |
| `secret … MISSING` | Steps 3 and 4 below |

Note the `Tailscale SSH policy users` line preflight prints at the end. That is your POSIX name, used next.

## 3. Tailscale admin console, once

Access controls opens in the visual editor. The JSON equivalents are in each step for the **JSON editor** tab; there, merge them into the one existing `{ … }` rather than pasting them alone.

1. Access controls → **Tags** → Create tag: name `devbox`, owner `autogroup:admin`.
   JSON: `"tagOwners": { "tag:devbox": ["autogroup:admin"] }`
2. Settings → **Trust credentials** → Credential → OAuth: scope **Auth Keys (write)**, tag `tag:devbox`, then Generate credential. Copy the client secret; it is shown once.
3. Store it:
   ```bash
   printf '%s' 'PASTE-THE-OAUTH-SECRET' | gcloud secrets create devbox-tailscale-oauth --data-file=- --labels=devbox=devbox
   ```
4. Access controls → **Tailscale SSH** → Add rule: source `autogroup:member`, destination `tag:devbox`, users `YOUR_POSIX_NAME` and `root` (from the preflight line), action accept.
   JSON: `"ssh": [{ "action": "accept", "src": ["autogroup:member"], "dst": ["tag:devbox"], "users": ["YOUR_POSIX_NAME", "root"] }]`
5. Optional, for the Finder mount in [05-files.md](05-files.md), in the JSON editor:
   ```json
   "nodeAttrs": [
     { "target": ["tag:devbox"],       "attr": ["drive:share"] },
     { "target": ["autogroup:member"], "attr": ["drive:access"] }
   ],
   "grants": [{ "src": ["autogroup:member"], "dst": ["tag:devbox"],
                "app": { "tailscale.com/cap/drive": [{ "shares": ["out"], "access": "rw" }] } }]
   ```

## 4. Machine key

```bash
ssh-keygen -t ed25519 -N '' -C 'devbox' -f devbox_ed25519
gcloud secrets create devbox-ssh-key --data-file=devbox_ed25519 --labels=devbox=devbox
gh auth refresh -s admin:public_key,admin:ssh_signing_key
gh ssh-key add devbox_ed25519.pub --type authentication --title devbox
gh ssh-key add devbox_ed25519.pub --type signing --title devbox
shred -u devbox_ed25519 2>/dev/null || rm -P devbox_ed25519; rm devbox_ed25519.pub
```

Both `gh ssh-key add` lines are needed: one for pushes, one so commits show Verified.

## 5. Up

```bash
./devbox preflight          # every line ok
./devbox up
```

Provisioning runs for a few minutes after `up` returns. Watch it through IAP, which works before the tailnet does:

```bash
gcloud compute ssh devbox --zone europe-west4-a --tunnel-through-iap -- sudo journalctl -fu devbox-provision
```

It ends with `=== provision done`.

## 6. First SSH

The first login goes through IAP, because it creates your account on the box:

```bash
gcloud compute ssh devbox --zone europe-west4-a --tunnel-through-iap
id -un                      # YOUR_POSIX_NAME, for the commands below
exit
```

If it fails with `4003: 'failed to connect to backend'`, the box has suspended itself after an idle hour: `./devbox up`, then retry.

From then on, over the tailnet:

```bash
ssh YOUR_POSIX_NAME@devbox
ssh-add -L                  # the devbox key, from the system agent
sudo -v                     # osAdminLogin gives you sudo
```

For daily use: `mosh YOUR_POSIX_NAME@devbox -- tmux new -A -s main`.

If mosh says `did not make a successful connection`, the macOS firewall is dropping replies to the unsigned Homebrew `mosh-client`. Allow it, and again after every `brew upgrade mosh`, since the path carries the version:

```bash
mc=$(realpath /opt/homebrew/bin/mosh-client)
sudo /usr/libexec/ApplicationFirewall/socketfilterfw --add "$mc"
sudo /usr/libexec/ApplicationFirewall/socketfilterfw --unblockapp "$mc"
```

## 7. Claude Code login

On the box, run `claude`. Press `c` to copy the login URL, open it on the laptop, and if the browser shows a code, paste it at `Paste code here if prompted`. The login is stored on `/srv/code`, so a rebuilt box keeps it. Put the expiry date Claude shows into your calendar.

**Done** when `claude` starts without asking you to log in and `git commit --allow-empty -m test` in a scratch repo shows `Good "git" signature` under `git log --show-signature`.
