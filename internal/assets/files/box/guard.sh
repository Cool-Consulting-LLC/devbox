#!/bin/bash
# PreToolUse hook: deny commits, pushes and applies, including the whitespace,
# quoting, eval and sh -c variants that permission globs miss. Policy, not a boundary.
cmd=$(jq -r '.tool_input.command // ""')
pre='(^|[;&|(`{!]|\$\(|["'"'"']|eval[[:space:]]+|exec[[:space:]]+|xargs[[:space:]]+|env[[:space:]]+|sudo[[:space:]]+|command[[:space:]]+|[[:space:]]-c[[:space:]]+)'
gitopt='([[:space:]]+(-C|-c|--git-dir|--work-tree)[[:space:]]*[^[:space:]]+|[[:space:]]+--[a-z-]+(=[^[:space:]]+)?)*'
verbs="(git${gitopt}[[:space:]]+(commit|push)|terraform([[:space:]]+-chdir=[^[:space:]]+)?[[:space:]]+(apply|destroy)|devbox([[:space:]]+-[^[:space:]]+)*[[:space:]]+(apply|destroy))"
if grep -qE "${pre}[[:space:]]*([^[:space:];&|]*/)?${verbs}([[:space:]]|$|[;&|)\`'\"])" <<<"$cmd"; then
  jq -n '{hookSpecificOutput:{hookEventName:"PreToolUse",
          permissionDecision:"deny",
          permissionDecisionReason:"Commits, pushes and applies are yours."}}'
fi
exit 0
