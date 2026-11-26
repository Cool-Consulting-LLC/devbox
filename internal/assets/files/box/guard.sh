#!/bin/bash
# PreToolUse hook: deny commits, pushes and applies, remote shells and copies,
# and any AWS call that is not a read, including the whitespace, quoting, eval
# and sh -c variants that permission globs miss. Policy, not a boundary.
cmd=$(jq -r '.tool_input.command // ""')
pre='(^|[;&|(`{!]|\$\(|["'"'"']|eval[[:space:]]+|exec[[:space:]]+|xargs[[:space:]]+|env[[:space:]]+|sudo[[:space:]]+|command[[:space:]]+|nohup[[:space:]]+|time[[:space:]]+|timeout[[:space:]]+[^[:space:]]+[[:space:]]+|[[:space:]]-c[[:space:]]+)'
# Then any VAR=value assignments and a directory.
at="${pre}[[:space:]]*([A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]+)*([^[:space:];&|]*/)?"
end='([[:space:]]|$|[;&|)`'"'"'"])'
deny() {
  jq -n --arg r "$1" '{hookSpecificOutput:{hookEventName:"PreToolUse",
          permissionDecision:"deny", permissionDecisionReason:$r}}'
  exit 0
}

gitopt='([[:space:]]+(-C|-c|--git-dir|--work-tree)[[:space:]]*[^[:space:]]+|[[:space:]]+--[a-z-]+(=[^[:space:]]+)?)*'
verbs="(git${gitopt}[[:space:]]+(commit|push)|terraform([[:space:]]+-chdir=[^[:space:]]+)?[[:space:]]+(apply|destroy)|devbox([[:space:]]+-[^[:space:]]+)*[[:space:]]+(apply|destroy))"
grep -qE "${at}${verbs}${end}" <<<"$cmd" && deny "Commits, pushes and applies are yours."

# rsync is remote when an argument is host:path or rsync://, or it names a shell.
remote="((ssh|scp|sftp|mosh)${end}|rsync([[:space:]][^;&|]*)?[[:space:]]([^[:space:]/:]+:|rsync://|-[a-zA-Z]*e|--rsh)"
remote+="|gcloud([[:space:]]+[^[:space:];&|]+)*[[:space:]]+compute([[:space:]]+[^[:space:];&|]+)*[[:space:]]+(ssh|scp)${end})"
grep -qE "${at}${remote}" <<<"$cmd" && deny "Remote shells and file copies are yours."

# Each aws call, up to the end of its command: its global options, then the
# service and operation must read as a read. Anything unparsed is denied.
readonly_msg="Only read-only AWS commands run here (describe-/list-/get-/lookup-)."
secret_msg="This AWS command returns secrets or credentials."
while IFS= read -r m; do
  rest=$m
  while [[ $rest == *aws* ]]; do
    rest=${rest#*aws}
    [[ -z $rest || $rest == [[:space:]\"\']* ]] && break
  done
  read -ra t <<<"${rest//[\"\']/ }"
  i=0 version=
  while [[ ${t[i]-} == -* ]]; do
    case ${t[i]} in
      --profile|--region|--output|--query|--endpoint-url|--color|--ca-bundle|--cli-read-timeout|--cli-connect-timeout|--cli-binary-format) i=$((i + 2)) ;;
      --profile=*|--region=*|--output=*|--query=*|--endpoint-url=*|--color=*|--ca-bundle=*|--cli-read-timeout=*|--cli-connect-timeout=*|--cli-binary-format=*) i=$((i + 1)) ;;
      --debug|--no-cli-pager|--no-paginate|--no-verify-ssl|--no-sign-request|--cli-auto-prompt|--no-cli-auto-prompt) i=$((i + 1)) ;;
      --version) version=1 i=$((i + 1)) ;;
      *) deny "$readonly_msg" ;;
    esac
  done
  svc=${t[i]-} op=${t[i + 1]-}
  [[ -z $svc && -n $version ]] && continue
  [[ $svc =~ ^[a-z0-9][a-z0-9-]*$ && $op =~ ^[a-z0-9][a-z0-9-]*$ ]] || deny "$readonly_msg"
  case "$svc $op" in
    "ssm start-session"|"ssm send-command") deny "SSM sessions and commands are yours." ;;
    "sts get-caller-identity"|"s3 ls"|"logs tail"|"configure list") ;;
    "sts "*|"secretsmanager get-secret-value"|"ecr get-login-password"|"ecr-public get-login-password"|"eks get-token"|\
    "codeartifact get-authorization-token"|"sso get-role-credentials"|"ec2 get-password-data"|"lightsail get-instance-access-details")
      deny "$secret_msg" ;;
    "ssm get-parameter"|"ssm get-parameters"|"ssm get-parameters-by-path")
      [[ " ${t[*]} " == *" --with-decryption"* ]] && deny "$secret_msg" ;;
    *) [[ $op =~ ^(describe|list|get|lookup)- ]] || deny "$readonly_msg" ;;
  esac
done < <(grep -oE "${at}aws([[:space:]][^;&|)\`]*|[;&|)\`'\"]|$)" <<<"$cmd")
exit 0
