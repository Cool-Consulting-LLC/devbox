# shellcheck shell=sh
# /etc/profile.d/devbox.sh, written by provision.sh. Applies to every login.
export SSH_AUTH_SOCK=/run/devbox/ssh-agent.sock
# Your own rootless Docker daemon; there is no system one.
if [ "$(id -u)" != 0 ]; then
  DOCKER_HOST="unix:///run/user/$(id -u)/docker.sock"
  export DOCKER_HOST
fi

# Nobody has sudo on the box except for devbox-enroll, which sets up an OS
# Login admin: home directory (sshd's PAM creates it, Tailscale SSH does not,
# and logs in with HOME=/), linger, and an ACL on /srv/code. Non-admins are
# refused by it and left alone.
if [ -n "${USER:-}" ] && [ "$USER" != root ]; then
  __devbox_home=$(getent passwd "$USER" | cut -d: -f6)
  if [ -d /srv/code/.devbox ] && { [ ! -e "/srv/code/.devbox/enrolled/$(id -u)" ] || { [ -n "$__devbox_home" ] && [ ! -d "$__devbox_home" ]; }; }; then
    sudo -n /usr/local/sbin/devbox-enroll >/dev/null 2>&1 || true
  fi
  if [ -n "$__devbox_home" ] && [ -d "$__devbox_home" ] && [ "$HOME" != "$__devbox_home" ]; then
    HOME=$__devbox_home
    export HOME
    cd "$HOME" || true
  fi
  unset __devbox_home
fi

# Anything that must survive a replacement lives on /srv/code and is linked in.
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
