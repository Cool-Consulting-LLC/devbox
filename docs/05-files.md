# 05 · Files

**How do I get a file in or out?**

| The job | Use |
|---|---|
| Grab a file off the box, now | `tailscale serve` on `/srv/code/_out`, then `curl -C -` |
| Push or pull a directory | rsync over SSH |
| Keep a folder continuously in step | Syncthing |
| Browse and edit a few files from Finder | Tailscale Drive |
| Something enormous, or a client handoff | A GCS bucket |

Shell over mosh; files over ssh. rsync does not work over mosh.

## Pull one file

On the box:

```bash
cp ~/report.pdf /srv/code/_out/
sudo tailscale serve --bg /srv/code/_out
tailscale status --json | jq -r '.Self.DNSName'      # e.g. devbox.tail1234.ts.net.
```

On the laptop, with that name minus the trailing dot:

```bash
curl -fC - -O https://devbox.tail1234.ts.net/report.pdf
```

Re-run the same `curl` to resume. Stop serving with `sudo tailscale serve --https=443 off`. Serve `_out` only, never `/srv/code`. HTTPS certificates must be enabled in the tailnet's DNS settings.

## Push or pull a directory

```bash
rsync -aHAX --info=progress2 --partial-dir=.rsync-partial \
      -z --compress-level=3 --timeout=120 \
      -e 'ssh -o ControlMaster=auto -o ControlPath=~/.ssh/cm-%C \
          -o ControlPersist=10m -o ServerAliveInterval=15' \
      ./dataset/ YOUR_POSIX_NAME@devbox:/srv/code/proj/dataset/
```

Swap the last two arguments to pull. Re-run to resume. Use current rsync on both ends (3.5.0 or later).

## Keep a folder in step

On the box: `systemctl --user enable --now syncthing`, then on the laptop forward its GUI with `ssh -L 8385:127.0.0.1:8384 YOUR_POSIX_NAME@devbox` and open http://127.0.0.1:8385. In both GUIs, turn off global discovery and relaying, and add the other device by ID with the address `tcp://devbox:22000` (or the laptop's tailnet name).

## Finder mount

Requires step 5 of [01-first-run.md](01-first-run.md). On the box:

```bash
sudo tailscale set --operator=$USER                 # once: lets you manage Tailscale without sudo
tailscale drive share out /srv/code/_out            # served as you, so files stay yours
```

Both settings live in Tailscale's state on the pet disk and survive replacements.

On the Mac: Finder → ⌘K → `http://100.100.100.100:8080` → Connect (as Guest if asked), then open the tailnet's folder, then `devbox`, then `out`. Drag `out` to the sidebar to keep it.

## Something enormous

```bash
gcloud storage buckets create gs://YOUR-BUCKET --location=europe-west4 --uniform-bucket-level-access
gcloud storage cp -r /srv/code/_out/bigthing gs://YOUR-BUCKET/       # on the box, using your gcloud login
gcloud storage cp -r gs://YOUR-BUCKET/bigthing .                      # anywhere
```

Pick `YOUR-BUCKET` yourself; bucket names are global. Delete it when done: `gcloud storage rm -r gs://YOUR-BUCKET`.
