#!/bin/bash
# Claude Code hook: records whether this session is working, for idle-check.sh.
# One file per session in /run/devbox-claude holding "<state> <claude pid>". A
# session is working from a prompt until it stops, and waiting while a
# permission prompt is up, because then it is your keyboard the box waits on.
dir=/run/devbox-claude
in=$(cat)
sid=$(jq -r '.session_id // ""' <<<"$in")
event=$(jq -r '.hook_event_name // ""' <<<"$in")
[[ $sid =~ ^[A-Za-z0-9-]+$ ]] && [ -d "$dir" ] || exit 0
f="$dir/$sid"
case "$event" in
  Stop|SessionEnd) rm -f "$f"; exit 0 ;;
  Notification) state=waiting ;;
  *) state=working ;;
esac
# The claude process that ran this hook, so that a session that died without a
# Stop does not keep the box up.
pid=$PPID
while [ "${pid:-0}" -gt 1 ] && [ "$(cat /proc/"$pid"/comm 2>/dev/null)" != claude ]; do
  pid=$(ps -o ppid= -p "$pid" | tr -d ' ')
done
[ "${pid:-0}" -gt 1 ] && echo "$state $pid" > "$f"
exit 0
