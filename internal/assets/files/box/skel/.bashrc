# shellcheck shell=bash disable=SC1090  # sources files that only exist on the box
# ~/.bashrc on the devbox, from /etc/skel. Ported from the laptop's ~/.bashrc:
# macOS-only parts dropped, GPG agent lines replaced by the system ssh-agent socket.
# `make diff-dotfiles` shows drift against the original.

# If not running interactively, don't do anything
case $- in
    *i*) ;;
      *) return;;
esac

########  paths

export GOPATH=$HOME/go
export PATH="$HOME/bin:$HOME/.local/bin:$PATH:/usr/local/go/bin:$GOPATH/bin"

####### bash options

HISTCONTROL=ignoreboth
shopt -s histappend
shopt -s checkwinsize

export HISTSIZE=200000
export HISTFILESIZE=400000
export HISTTIMEFORMAT="%h/%d -- %H:%M:%S "

if ! shopt -oq posix; then
  if [ -f /usr/share/bash-completion/bash_completion ]; then
    . /usr/share/bash-completion/bash_completion
  elif [ -f /etc/bash_completion ]; then
    . /etc/bash_completion
  fi
fi

[ -f ~/.bash_aliases ] && . ~/.bash_aliases

########  aliases and tools configs

export GCC_COLORS='error=01;31:warning=01;35:note=01;36:caret=01;32:locus=01:quote=01'
export EDITOR=vim
export GITPAGER=less
[ -x /usr/bin/lesspipe ] && eval "$(SHELL=/bin/sh lesspipe)"

# The machine key lives in the system agent; /etc/profile.d/devbox.sh sets this too.
export SSH_AUTH_SOCK=/run/devbox/ssh-agent.sock

if [ -x /usr/bin/dircolors ]; then
    if [ -r ~/.dircolors ]; then eval "$(dircolors -b ~/.dircolors)"; else eval "$(dircolors -b)"; fi
    alias ls='ls --color=auto'
    alias grep='grep --color=auto'
fi

command -v kubectl >/dev/null && source <(kubectl completion bash)
command -v direnv >/dev/null && eval "$(direnv hook bash)"

alias la='ls -A'
alias l='ls -CF'
alias ll='ls -lah'
alias ipa='ip a'
alias fd=fdfind

alias gs='git status'
alias gr='git pull --rebase'
alias gd='git diff'
alias gds='git diff --staged'
alias ga='git add .'

gc() { git commit -m "$1"; }
gp() { git push "$@"; }
gt() { git tag "v$(< VERSION)" && sleep 2 && git push origin "v$(< VERSION)"; }

alias tl='tmux list-sessions'
# Sessions in columns: ● attached (green), windows, last activity. Coloured
# after column(1), which would count the escape codes as width.
ts() {
  tmux list-sessions -F '#{?session_attached,●, } #{session_name}	#{session_windows}w	#{t/p:session_activity}' 2>/dev/null \
    | column -t -s $'\t' | sed $'s/●/\e[32m●\e[0m/'
}
# Attach to the session whose name ends in $1: `ta api` finds acme-api.
ta() { tmux attach -t "*$1"; }

########  prompt

_branch_show() {
  local b
  b=$(git symbolic-ref --short HEAD 2>/dev/null) && printf '%s ' "$b"
}

# A stable, readable colour per hostname, so the devbox never looks like the laptop.
_foreground_calc() {
  # reject these colors for being too dark
  local reject=([0]=a [16]=a [17]=a [18]=a [19]=a [232]=a [233]=a [234]=a [235]=a [236]=a [237]=a [238]=a)
  local num
  echo "$1" | sha1sum | while read -r -n2 num; do
    num=$((16#$num))
    if [ -z "${reject[$num]}" ]; then
      echo "$num"
      return
    fi
  done
}
hostnamecolor=$(_foreground_calc "$HOSTNAME")
PS1='\[\e[32m\]\u@\[\e[38;5;${hostnamecolor}m\]\h \[\e[32m\]\w \[\e[33m\]$(_branch_show)\[\e[32m\]\j\[\e[0m\] \$ '

######## let's gooo

if [ -z "${TMUX:-}" ]; then
  uptime
  echo
  ts
fi
