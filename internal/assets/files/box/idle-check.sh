#!/bin/bash
# Run by devbox-idle.timer every 5 minutes, as root. Suspends the box through the
# Compute API after THRESHOLD minutes with no keyboard input, no Claude Code turn
# in progress and no non-interactive SSH session.
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

# Being connected is not being busy: a laptop left awake with mosh open would keep
# the box up all night. What counts is keyboard input on any terminal (the tty's
# atime, as in `w`), which covers ssh, mosh and tmux alike.
last_input=0
for tty in /dev/pts/[0-9]*; do
  [ -c "$tty" ] || continue
  t=$(stat -c %X "$tty")
  [ "$t" -gt "$last_input" ] && last_input=$t
done

# Claude Code sessions mid-turn, from the claude-activity.sh hook. A working
# file only counts while its claude process lives, and for at most MAX_TURN
# minutes since the last hook, in case a turn ends without a Stop.
MAX_TURN=180
claude_working=0
claude_waiting=0
for f in /run/devbox-claude/*; do
  [ -f "$f" ] && [ ! -L "$f" ] || continue
  read -r state pid < "$f" 2>/dev/null
  if [[ ! $pid =~ ^[0-9]+$ ]] || [ "$(cat /proc/"$pid"/comm 2>/dev/null)" != claude ]; then
    rm -f "$f"
    continue
  fi
  [ $(( now - $(stat -c %Y "$f") )) -lt $(( MAX_TURN * 60 )) ] || continue
  case "$state" in
    working) claude_working=$(( claude_working + 1 )) ;;
    waiting) claude_waiting=$(( claude_waiting + 1 )) ;;
  esac
done

# Sessions without a terminal have no keyboard to watch: `devbox reprovision`,
# scp, port forwards. They count while connected; they end when the laptop sleeps.
ssh_notty=$(pgrep -fc '^sshd(-session)?: [^ ]+@notty' || true)
ts_notty=$(pgrep -fc '^/usr/sbin/tailscaled be-child ssh .*--has-tty=false' || true)

busy=$(( claude_working + ssh_notty + ts_notty ))
input_min=$(( ( now - last_input ) / 60 ))
# What the last check saw, for the next "why did (or didn't) it suspend?".
echo "$(date -Is) input=${input_min}m claude_working=$claude_working claude_waiting=$claude_waiting ssh_notty=$ssh_notty ts_notty=$ts_notty busy=$busy" > /run/devbox-idle.status
if [ "$busy" -gt 0 ]; then
  echo "$now" > /run/devbox-last-active
  exit 0
fi
if [ "$last_input" -gt "$(cat /run/devbox-last-active 2>/dev/null || echo 0)" ]; then
  echo "$last_input" > /run/devbox-last-active
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
