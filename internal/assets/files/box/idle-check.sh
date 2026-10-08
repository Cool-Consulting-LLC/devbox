#!/bin/bash
# Run by devbox-idle.timer every 5 minutes, as root. Suspends the box through the
# Compute API after THRESHOLD idle minutes. A detached tmux session is not busy.
THRESHOLD=${1:-60}
now=$(date +%s)

# A gap since the last run means we were suspended. Start the idle clock again,
# or a resumed box would suspend itself before anyone could connect.
last_check=$(cat /run/devbox-last-check 2>/dev/null || echo 0)
echo "$now" > /run/devbox-last-check
if [ $(( now - last_check )) -gt 900 ]; then
  echo "$now" > /run/devbox-last-active
  exit 0
fi

# This runs as root, so `tmux list-clients` alone would only see root's
# server. Walk every user's socket instead.
tmux_clients=0
for sock in /tmp/tmux-*/default; do
  [ -S "$sock" ] || continue
  n=$(tmux -S "$sock" list-clients 2>/dev/null | wc -l)
  tmux_clients=$(( tmux_clients + n ))
done

# Established connections to port 22 cover sshd (IAP, break-glass) and the mosh
# bootstrap. Tailscale SSH terminates inside tailscaled's netstack, where ss may
# not see it, so also count its per-session `tailscaled be-child ssh` processes.
ssh_conns=$(ss -Htn state established '( sport = :22 )' | wc -l)
ts_ssh=$(pgrep -fc '^/usr/sbin/tailscaled be-child ssh' || true)
mosh_conns=$(pgrep -c mosh-server || true)
claude_procs=$(pgrep -c -x claude || true)

busy=$(( tmux_clients + ssh_conns + ts_ssh + mosh_conns + claude_procs ))
# What the last check saw, for the next "why did it suspend?".
echo "$(date -Is) tmux=$tmux_clients ssh=$ssh_conns ts_ssh=$ts_ssh mosh=$mosh_conns claude=$claude_procs busy=$busy" > /run/devbox-idle.status
if [ "$busy" -gt 0 ]; then
  echo "$now" > /run/devbox-last-active
  exit 0
fi

last=$(cat /run/devbox-last-active 2>/dev/null || echo "$now")
[ -f /run/devbox-last-active ] || echo "$now" > /run/devbox-last-active
idle_min=$(( ( now - last ) / 60 ))
[ "$idle_min" -lt "$THRESHOLD" ] && exit 0

md() { curl -fsS -H 'Metadata-Flavor: Google' "http://metadata.google.internal/computeMetadata/v1/$1"; }
zone=$(md instance/zone | cut -d/ -f4)
name=$(md instance/name)
tok=$(md instance/service-accounts/default/token | jq -r .access_token)
logger -t devbox-idle "idle ${idle_min}m >= ${THRESHOLD}m, suspending ($(cat /run/devbox-idle.status))"
curl -fsS -X POST -H "Authorization: Bearer $tok" -H 'Content-Length: 0' \
  "https://compute.googleapis.com/compute/v1/projects/$(md project/project-id)/zones/$zone/instances/$name/suspend" >/dev/null
