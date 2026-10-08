# 04 · Changes

**How do I change something without losing my session?**

## The three tiers

| Tier | Cost | What lives here | How |
|---|---|---|---|
| **Live** | Nothing | Anything under `/srv/code`, `.devbox-env` files, `~/.tmux.conf`, Claude settings, `/etc/skel` for future accounts | Edit on the box |
| **Reprovision** | ~30 s, nothing lost | `provision.sh` and everything it writes: dotfiles, helpers, idle check, the agent key, Tailscale, Claude Code, git config, `extra_packages`. Disk growth | Edit the repo, `make build`, `./devbox reprovision` |
| **Replace** | ~2 min, sessions and home directories lost | `cloud-init.yaml`: base packages, systemd units, sshd config, managed settings, the guard hook. Network or subnet | Edit the repo, `make build`, `./devbox apply` |

`machine_type` sits between: `apply` stops and starts the instance. Disks survive, memory does not.

Which tier each `devbox.yaml` key is in: [02-config.md](02-config.md).

## reprovision or apply?

- `./devbox apply --dry-run` prints what `apply` would do and changes nothing. Start here when unsure.
- `apply` changes GCP. Lines marked `-/+` (replace) or `~!` (restart) end sessions; it asks first.
- `reprovision` changes the box. It updates instance metadata from `devbox.yaml`, pushes `provision.sh` over SSH, and runs it. It never replaces anything.
- When `apply` prints `run devbox reprovision`, do that next.
- Both resume a suspended box before changing it.
- If the tailnet is down: `./devbox reprovision --iap`.

## Replacement-tier changes: use a throwaway box

```bash
make build
./devbox up --name devbox-test --code-disk-gb 20
ssh YOUR_POSIX_NAME@devbox-test           # iterate until the cloud-init change is right
./devbox destroy --name devbox-test --all
./devbox apply                            # now replace the real one
```

`--name` gives the test box its own instance, disk, service account, firewall rules and Tailscale hostname. It shares the project's `devbox-*` secrets.

Anything fixed by hand over SSH is gone at the next replacement. If you needed to do it once, put it in `provision.sh`.
