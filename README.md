# devbox

A single static binary that stands up a Claude Code workstation in a GCP project
you own, reachable only over your tailnet, with IAP and a break-glass firewall
rule as second ways in, a machine identity that keeps your personal key out of
the cloud, and a destroy path that returns the project to how it started. No
Terraform, no state file.

Full design: [`docs/plan.md`](docs/plan.md).

## Status

**Built, not yet run against a real project.** Every phase in the plan is
implemented and unit-tested; the acceptance test is
[docs/01-first-run.md](docs/01-first-run.md) on a clean laptop.

| | |
|---|---|
| ✅ Phase 0 | The binary: `preflight`, `up`, `down`, `status`, `apply`, `reprovision`, `project`, `destroy` |
| ✅ Phase 1 | Preflight, with `--fix` for APIs |
| ✅ Phase 2–4 | Firewalls, service account and IAM, disks, snapshot schedule, the instance and its lifecycle |
| ✅ Phase 5 | cloud-init, `provision.sh`, sshd, Tailscale, Claude Code from the signed apt repo |
| ✅ Phase 6–7 | Sessions, per-project env and secrets, managed settings and the guard hook |
| ✅ Phase 9–10 | Idle suspend, alert policies |

Things worth knowing before the first `up`:

- **The reconciler has only met a fake.** `up`, `apply` and `destroy` are tested
  end to end against an in-memory GCP that enforces the ordering rules (resume
  before edit, stop before `setMachineType`, no deleting attached disks), and
  that fake caught a real bug. It is not Google. `preflight` is not covered by
  it at all.
- **The box-side scripts have been parsed and linted, never run on Ubuntu.**
  The first `journalctl -fu devbox-provision` is their first execution.
- **Some claims are unverified until they are live:** the disk alert assumes the
  code disk appears as `sdb`; the idle alert uses a 6-hour duration; Tailscale
  SSH sessions are counted by process because `ss` may not see them. The plan's
  sharp-edges table lists the rest.

## Quick start

```bash
make setup        # modules, ./devbox, and devbox.yaml from the example
make auth         # gcloud login and Application Default Credentials
./devbox preflight --fix
```

Then [docs/01-first-run.md](docs/01-first-run.md) from step 3.

```
devbox preflight              # read-only survey; --fix enables what it can
devbox up                     # create what is missing, resume if suspended
devbox down                   # suspend, preserving memory state
devbox status                 # what exists, its state, what it is costing
devbox apply [--dry-run]      # reconcile config to reality; prints the diff first
devbox reprovision            # push provision.sh from the repo and run it
devbox project add <repo>     # clone and register a project on the box
devbox project secret <p> <K> # store a project secret, reference it from the env
devbox destroy [--all]        # tear down; --all includes the code disk
```

## Runbooks

| Runbook | Question |
|---|---|
| [01-first-run](docs/01-first-run.md) | From nothing to a shell on the box? |
| [02-config](docs/02-config.md) | What does each value in `devbox.yaml` do? (generated) |
| [03-teardown](docs/03-teardown.md) | How do I bring it down cleanly, and know it is down? |
| [04-changes](docs/04-changes.md) | How do I change something without losing my session? |
| [05-files](docs/05-files.md) | How do I get a file in or out? |
| [06-projects-and-secrets](docs/06-projects-and-secrets.md) | How do I add a project, and give it a secret? |

The runbooks hold steps; [the plan](docs/plan.md) holds every reason.

## Architecture

```
cmd/devbox/                  main
internal/config/             devbox.yaml: parsing, validation, the generated reference
internal/box/                one planner per resource; up, apply, destroy, status, preflight
                             (fake_test.go is the in-memory GCP the flow tests run against)
internal/gcp/                API clients and operation helpers
internal/remote/             SSH to the box (tailnet, or IAP with --iap)
internal/cli/                the commands; help text must agree with the runbooks
internal/assets/files/       everything that runs on the box, embedded in the binary
  cloud-init.yaml            replace tier
  bootstrap.sh               replace tier: first-boot disk format, Google apt repos
  managed-settings.json      replace tier: Claude Code guardrails
  guard.sh                   replace tier: PreToolUse hook
  provision.sh               reprovision tier
  box/                       reprovision tier: helpers, dotfiles, idle check
docs/                        the plan and the six runbooks
```

## Development

```bash
make              # every target, grouped
make check        # lint + test + docs-check, the same as CI
make docs         # regenerate docs/02-config.md; never edit it by hand
make release      # dist/ for darwin and linux, amd64 and arm64
make diff-dotfiles  # the box's .bashrc against your ~/.bashrc
```

`make test` needs no credentials and no network. The guard-hook test needs `jq`;
`make lint` shellchecks the box scripts when `shellcheck` is installed, and CI
always has it.
