#!/bin/bash
# Claude Code hook: records whether this session is working, for idle-check.sh.
# One file per session in /run/devbox-claude holding "<state> <claude pid>". A
# session is working from a prompt until it stops, and waiting while a
# permission prompt is up, because then it is your keyboard the box waits on.
# A turn cut off by the 5-hour usage limit is resuming until the limit resets
# and devbox-claude-resume types "continue": "resuming <pid> <resume_at>".
dir=/run/devbox-claude
in=$(cat)
sid=$(jq -r '.session_id // ""' <<<"$in")
event=$(jq -r '.hook_event_name // ""' <<<"$in")
[[ $sid =~ ^[A-Za-z0-9-]+$ ]] && [ -d "$dir" ] || exit 0
f="$dir/$sid"
case "$event" in
  Stop|SessionEnd) rm -f "$f"; exit 0 ;;
  StopFailure)
    if resume_at=$(/usr/local/bin/devbox-claude-resume <<<"$in"); then
      state="resuming"
    else
      rm -f "$f"; exit 0
    fi ;;
  Notification) state=waiting ;;
  *) state=working ;;
esac
# The claude process that ran this hook, so that a session that died without a
# Stop does not keep the box up.
pid=$PPID
while [ "${pid:-0}" -gt 1 ] && [ "$(cat /proc/"$pid"/comm 2>/dev/null)" != claude ]; do
  pid=$(ps -o ppid= -p "$pid" | tr -d ' ')
done
# Only admins can write here; for anyone else this fails, and that is fine.
[ "${pid:-0}" -gt 1 ] && echo "$state $pid${resume_at:+ $resume_at}" 2>/dev/null > "$f"
exit 0
