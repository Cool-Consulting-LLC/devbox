# devbox

This repo is the source of the `devbox` CLI, a Go binary that builds and
provisions a Claude Code workstation on GCP. Paths below are relative to the
repo root.

On a devbox, this repo is usually checked out as a project's repo, under
`/srv/code/<project>/`, and the project root's `CLAUDE.md` is a link to this
file. The project root is not a repository; run git inside the repo.

## Change the code, not this machine

Every request here is about the codebase, even when it is phrased as if about
the machine ("make the statusline show X", "add a package", "fix the idle
check", "change the prompt"). Do not edit `/etc`, `/usr/local/bin`, `/etc/claude-code`, `~/.bashrc`,
systemd units, crontabs or anything else outside the repo, and do not install
packages or restart services to make a change take effect. Ask first if a
request really is about this machine only.

What lands on a box comes from the repo:

- `internal/assets/files/box/`: the scripts, hooks, configs and dotfiles
  installed on the box (e.g. `/usr/local/bin/devbox-*`,
  `/etc/claude-code/managed-settings.json` and its hooks, `skel/` for the home
  directory's dotfiles, `tmux.conf`).
- `internal/assets/files/provision.sh`: packages, users, sudo, git and SSH
  setup; rerun in place by `devbox reprovision`.
- `internal/assets/files/cloud-init.yaml` and `bootstrap.sh`: first boot.
- The Go packages under `internal/`: the CLI that creates and changes the GCP
  resources.

## Finishing a change

Run `make check build` (lint, tests, docs). If config struct tags changed, run `make
docs` rather than editing `docs/config.md`. A new box script must be added to
`SCRIPTS` (or `PYTHON`) in the Makefile, or `assets_test.go` fails. The user
rolls changes out themselves with `devbox reprovision` or
`devbox apply`; do not run those.
