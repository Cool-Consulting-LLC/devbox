# 06 · Projects and secrets

**How do I add a project, and give it a secret?**

## Add a project

```bash
./devbox project add git@github.com:acme/api.git \
  --gcloud-config client-a --gcp-project acme-prod --region europe-west2
```

This clones into `/srv/code/acme-api/`, writes `/srv/code/acme-api/.devbox-env`, puts the same non-secret variables in `.claude/settings.local.json` for agent sessions, and creates a tmux session named `acme-api`. Re-run it with different flags to change the variables. `--env KEY=VALUE` adds anything else.

Then authenticate that gcloud identity once, on the box:

```bash
export CLOUDSDK_CONFIG=/srv/code/.gcloud/client-a
gcloud auth login --no-launch-browser
gcloud auth application-default login --no-launch-browser
```

Attach with `tmux attach -t acme-api`.

## Give it a secret

```bash
./devbox project secret acme-api OPENAI_API_KEY          # prompts, input hidden
pbpaste | ./devbox project secret acme-api DATABASE_URL  # or from stdin
```

This stores `devbox-acme-api-OPENAI_API_KEY` in Secret Manager and adds `OPENAI_API_KEY=@secret:devbox-acme-api-OPENAI_API_KEY` to `.devbox-env`. The box can read it immediately; no IAM change.

Then, on the box, restart the session so it picks the value up:

```bash
devbox-sessions --recreate acme-api
```

## Where values live

| Place | Holds secrets? |
|---|---|
| Secret Manager `devbox-<project>-<KEY>` | Yes. The only copy |
| `.devbox-env` | No. Only the `@secret:` reference |
| tmux session environment | Yes, in memory, resolved when the session is created |
| `.claude/settings.local.json` | Never. Nothing resolves `@secret:` there |
| `.env` in the project | Only if `.devbox-env` contains `DEVBOX_DOTENV=1`. Mode 600, on the pet disk |

Agents started inside the project's tmux session inherit its secrets. `.devbox-env`, `.env` and `settings.local.json` are added to the clone's `.git/info/exclude`.

## Rotate a secret

Run the same `devbox project secret` command with the new value. Older versions are disabled. Then `devbox-sessions --recreate acme-api` on the box.

## Remove a secret

```bash
./devbox project secret acme-api OPENAI_API_KEY --delete
```

This removes the line from `.devbox-env` and deletes the secret. Running sessions keep the old value until `devbox-sessions --recreate acme-api`.

## Remove a project

On the box: `tmux kill-session -t acme-api && rm -rf /srv/code/acme-api`. Then delete its secrets from the laptop:

```bash
gcloud secrets list --filter="name~devbox-acme-api-" --format='value(name)' | xargs -n1 gcloud secrets delete --quiet
```
