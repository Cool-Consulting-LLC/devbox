GITROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
SHELL := /bin/bash
.DEFAULT_GOAL := help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/Cool-Consulting-LLC/devbox/internal/cli.Version=$(VERSION)
# CGO off is what makes the binary static: one file on a fresh laptop, nothing
# else installed. -trimpath keeps the builder's home directory out of it.
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"
PLATFORMS ?= darwin/arm64 darwin/amd64 linux/amd64 linux/arm64
PREFIX ?= $(HOME)/.local

# Everything that runs on the box. Linted here as well as by the Go tests, so
# `make lint` names the file and line rather than a test. No -s: each file
# declares its shell, and profile.sh is POSIX sh, not bash. The Python relay is
# checked separately. assets_test.go fails if a box file is in neither list.
BOX := $(GITROOT)/internal/assets/files/box
SCRIPTS := $(sort $(wildcard $(GITROOT)/internal/assets/files/*.sh) \
             $(addprefix $(BOX)/,claude-activity.sh devbox-agent-load-key devbox-claude-statusline devbox-enroll devbox-gcloud-adopt devbox-gcloud-login devbox-project devbox-secret devbox-sessions devbox-tmux-colour guard.sh idle-check.sh mosh-server profile.sh skel/.bashrc))
PYTHON := $(BOX)/devbox-agent-relay

# Where the originals of the ported dotfiles live: your laptop's home.
DOTFILES ?= $(HOME)
SKEL := $(GITROOT)/internal/assets/files/box/skel

##@ Help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "; printf "\nUsage: make \033[36m<target>\033[0m [VAR=value ...]\n"} \
		/^[a-zA-Z0-9_-]+:.*## / {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2} \
		/^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)}' $(MAKEFILE_LIST)
	@echo

##@ Develop

.PHONY: setup test lint fmt docs docs-check check diff-dotfiles
setup: ## Everything a fresh clone needs: modules, the binary, a devbox.yaml to edit
	go mod download
	@$(MAKE) --no-print-directory build
	@[ -f devbox.yaml ] || { cp devbox.example.yaml devbox.yaml; echo "created devbox.yaml from the example"; }
	@echo
	@echo "  Set project_id, git.name, git.email and alerts.email in devbox.yaml, then"
	@echo "  follow README.md, How to set it up, from step 2."
	@echo

test: ## Run the test suite (no credentials or network; GCP is an in-memory fake)
	go test ./...

lint: ## Vet, check formatting, and shellcheck the box scripts if shellcheck is installed
	go vet ./...
	@unformatted=$$(gofmt -l $(GITROOT)); \
	if [ -n "$$unformatted" ]; then echo "not gofmt'd:"; echo "$$unformatted"; exit 1; fi
	@if command -v shellcheck >/dev/null; then shellcheck -S warning $(SCRIPTS); \
	else echo "shellcheck not installed; skipping the box scripts (CI runs it)"; fi
	@python3 -c 'import ast, sys; [ast.parse(open(f).read(), f) for f in sys.argv[1:]]' $(PYTHON)

fmt: ## Reformat Go
	gofmt -w $(GITROOT)

# docs/config.md is generated from the config struct tags, so it cannot
# drift from the code. A reference that can drift is worse than none.
docs: ## Regenerate docs/config.md. Never edit it by hand
	go run ./cmd/devbox config --describe > docs/config.md

docs-check: ## Fail if docs/config.md is stale
	@go run ./cmd/devbox config --describe | diff -u docs/config.md - \
		&& echo "docs/config.md is current"

check: lint test docs-check ## Lint + test + docs

# The box's .bashrc is a cleaned copy of the laptop's. Keep the drift visible.
diff-dotfiles: ## Diff the box's dotfiles against the originals in $(DOTFILES)
	-diff -u $(DOTFILES)/.bashrc $(SKEL)/.bashrc
	-diff -u $(DOTFILES)/.ssh/config $(SKEL)/.ssh/config
	-diff -u $(DOTFILES)/.gitconfig $(SKEL)/.gitconfig
	-diff -u $(DOTFILES)/.tmux.conf $(GITROOT)/internal/assets/files/box/tmux.conf

##@ Build

.PHONY: build install release clean
build: ## Build the static binary at ./devbox
	$(GOBUILD) -o devbox ./cmd/devbox

install: build ## Copy ./devbox to $(PREFIX)/bin (override with PREFIX=...)
	install -d $(PREFIX)/bin
	install -m 0755 devbox $(PREFIX)/bin/devbox
	@case ":$$PATH:" in *":$(PREFIX)/bin:"*) ;; *) echo "note: $(PREFIX)/bin is not on PATH";; esac

release: ## Build dist/devbox-<os>-<arch> for $(PLATFORMS)
	@set -euo pipefail; \
	for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "dist/devbox-$$os-$$arch"; \
		GOOS=$$os GOARCH=$$arch $(GOBUILD) -o dist/devbox-$$os-$$arch ./cmd/devbox; \
	done

clean: ## Remove ./devbox and dist/
	rm -rf devbox dist

##@ Credentials

.PHONY: auth
# The binary uses Application Default Credentials, not the gcloud CLI login,
# so both are needed: the CLI for the README's gcloud commands, ADC for devbox.
# --update-adc writes both from one browser round trip.
auth: ## Log in to gcloud and create Application Default Credentials
	gcloud auth login --update-adc
