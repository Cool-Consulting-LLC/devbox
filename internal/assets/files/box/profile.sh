# shellcheck shell=sh
# /etc/profile.d/devbox.sh, written by provision.sh. Applies to every login.
export SSH_AUTH_SOCK=/run/devbox/ssh-agent.sock

# sshd's PAM creates a missing home directory at login; Tailscale SSH does not,
# and logs in with HOME=/. Create it from /etc/skel and move in. Admins only.
if [ -n "${USER:-}" ] && [ "$USER" != root ]; then
  __devbox_home=$(getent passwd "$USER" | cut -d: -f6)
  if [ -n "$__devbox_home" ] && [ ! -d "$__devbox_home" ] && sudo -n /usr/sbin/mkhomedir_helper "$USER" 0077 2>/dev/null; then
    HOME=$__devbox_home
    export HOME
    cd "$HOME" || true
  fi
  unset __devbox_home
fi

# Anything that must survive a replacement lives on /srv/code and is linked in.
# OS Login grants sudo per user, not by group, so admins get /srv/code through an
# ACL that devbox-enroll sets once. Non-admins have no sudo, and are left alone.
if [ -n "${USER:-}" ] && [ "$USER" != root ] && [ -d /srv/code/.devbox ] && [ ! -e "/srv/code/.devbox/enrolled/$(id -u)" ]; then
  sudo -n /usr/local/sbin/devbox-enroll >/dev/null 2>&1 || true
fi

if [ -n "${USER:-}" ] && [ "$USER" != root ] && [ -w /srv/code ]; then
  __devbox_persist() { # persist <path in home> <path on the pet disk>
    [ -L "$1" ] && return 0
    mkdir -p "$2" "$(dirname "$1")"
    if [ -e "$1" ]; then cp -a "$1/." "$2/" 2>/dev/null; rm -rf "$1"; fi
    ln -s "$2" "$1"
  }
  __devbox_persist "$HOME/.claude" "/srv/code/.home/$USER/claude"
  __devbox_persist "$HOME/.config/gcloud" "/srv/code/.gcloud/default"
  export CLAUDE_CONFIG_DIR="/srv/code/.home/$USER/claude"
  unset -f __devbox_persist

  # One tmux session per project, created on first interactive login.
  case $- in *i*) [ -z "${TMUX:-}" ] && QUIET=1 devbox-sessions 2>&1 | sed 's/^/  /' ;; esac
fi
