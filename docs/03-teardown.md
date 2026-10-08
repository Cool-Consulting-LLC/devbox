# 03 · Teardown

**How do I bring it down cleanly, and know it is down?**

## Pick one

| Command | What is left | Cost afterwards (e2-standard-2) |
|---|---|---|
| `devbox down` | Everything; instance suspended, memory saved | ~$11/mo: disks + suspended RAM |
| `devbox destroy` | Code disk, its snapshots, `devbox-*` secrets, custom role `devbox.selfSuspend` | ~$6/mo for the 50 GB disk, plus snapshots |
| `devbox destroy --all` | Nothing it created. You type the disk name to confirm | $0 |

`devbox up` brings back any of them. After `destroy`, it reattaches the surviving disk and nothing in `/srv/code` is lost. A box suspended for 60 days is stopped by Google and loses its memory state, not its disks.

For a throwaway box, pass the same `--name` to both commands: `devbox destroy --name devbox-test --all`.

## Verify

```bash
./devbox status
```

Every row says `absent` (or `present` only for what you chose to keep) and the cost line matches the table. For an audit that does not trust the binary, run these with `NAME=devbox`:

```bash
NAME=devbox
gcloud compute instances list          --filter="labels.devbox=$NAME"
gcloud compute disks list              --filter="labels.devbox=$NAME"
gcloud compute snapshots list          --filter="labels.devbox=$NAME"
gcloud compute resource-policies list  --filter="name~^$NAME-"
gcloud compute firewall-rules list     --filter="name~^$NAME-"
gcloud iam service-accounts list       --filter="email~^$NAME@"
gcloud secrets list                    --filter="labels.devbox=$NAME"
gcloud alpha monitoring policies list  --filter="user_labels.devbox=\"$NAME\"" --format='value(displayName)'
gcloud alpha monitoring channels list  --filter="user_labels.devbox=\"$NAME\"" --format='value(displayName)'
gcloud iam roles describe devbox.selfSuspend --project "$(gcloud config get project)" --format='value(deleted)'
```

Empty output means gone. `destroy` never touches the project, the VPC, the subnet or which APIs are enabled.

The custom role is shared by every box in the project. After `--all` deletes it, `devbox up` within 7 days undeletes it. Between day 7 and day 37 the ID cannot be reused at all, and `up` will say so.

## Restore /srv/code from a snapshot

```bash
ZONE=europe-west4-a
gcloud compute snapshots list --filter="labels.devbox=devbox" --sort-by=~creationTimestamp --format='value(name,creationTimestamp)'
SNAP=PASTE-A-NAME-FROM-THE-LIST-ABOVE
./devbox destroy                                                    # frees the disk; keeps it and its snapshots
gcloud compute snapshots create devbox-code-before-restore --source-disk=devbox-code --source-disk-zone=$ZONE
gcloud compute disks delete devbox-code --zone=$ZONE
gcloud compute disks create devbox-code --zone=$ZONE --type=pd-balanced --source-snapshot="$SNAP" --labels=devbox=devbox
./devbox up
```

The box finds a filesystem on the disk and mounts it without formatting. Delete `devbox-code-before-restore` once you are happy.
