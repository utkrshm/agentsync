#!/usr/bin/env bash
# agent-sync bootstrap: set up and run the latest version on a fresh machine.
#
# Idempotent: safe to re-run after pulling new commits. It never force-updates
# the source checkout (fast-forward only) and never touches existing configs.
#
# Environment overrides:
#   AGENT_SYNC_REPO    git URL to clone            (default: this repo's origin)
#   AGENT_SYNC_SRC     where the checkout lives    (default: ~/.local/src/agentsync)
#   AGENT_SYNC_BIN     where binaries are placed   (default: ~/.local/bin)
#
# Usage:
#   ./setup.sh              # install/update + build
#   ./setup.sh --run-help   # print post-install usage instead of building
set -euo pipefail

REPO_URL="${AGENT_SYNC_REPO:-git@github.com:utkrshm/agentsync.git}"
SRC_DIR="${AGENT_SYNC_SRC:-$HOME/.local/src/agentsync}"
BIN_DIR="${AGENT_SYNC_BIN:-$HOME/.local/bin}"
GO_MIN="1.25"

log() { printf '\n[agent-sync-setup] %s\n' "$*"; }
die() { printf '[agent-sync-setup] ERROR: %s\n' "$*" >&2; exit 1; }

have() { command -v "$1" >/dev/null 2>&1; }

SUDO=""
if [ "$(id -u)" -ne 0 ] && have sudo; then SUDO="sudo"; fi

apt_install() {
    if have apt-get; then
        $SUDO apt-get update -qq && $SUDO apt-get install -y -qq "$@"
    else
        die "missing dependency: $* (install manually, then re-run)"
    fi
}

# --- 1. base toolchain -------------------------------------------------------
for tool in git curl tar; do
    have "$tool" || apt_install "$tool"
done

# --- 2. Go >= GO_MIN --------------------------------------------------------
go_ok() {
    have go || return 1
    [ "$(printf '%s\n' "$GO_MIN" "$(go env GOVERSION | sed 's/^go//')" |
        sort -V | head -1)" = "$GO_MIN" ]
}

if ! go_ok; then
    log "installing Go ${GO_MIN} (none/newer-not-found)"
    VER="$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)"
    TARBALL="go${VER}.linux-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/').tar.gz"
    curl -fsSL "https://go.dev/dl/${TARBALL}" -o "/tmp/${TARBALL}"
    if [ -w /usr/local ]; then
        rm -rf /usr/local/go && tar -C /usr/local -xzf "/tmp/${TARBALL}"
        export PATH="/usr/local/go/bin:$PATH"
    elif have sudo; then
        $SUDO rm -rf /usr/local/go && $SUDO tar -C /usr/local -xzf "/tmp/${TARBALL}"
        export PATH="/usr/local/go/bin:$PATH"
    else
        mkdir -p "$HOME/.local/go"
        tar -C "$HOME/.local" -xzf "/tmp/${TARBALL}"
        export PATH="$HOME/.local/go/bin:$PATH"
    fi
    go_ok || die "Go ${GO_MIN} still unavailable after install"
fi

# --- 3. source checkout: clone or fast-forward ------------------------------
if [ -d "${SRC_DIR}/.git" ]; then
    log "updating existing checkout at ${SRC_DIR} (fast-forward only)"
    git -C "$SRC_DIR" fetch origin
    git -C "$SRC_DIR" merge --ff-only origin/HEAD 2>/dev/null \
        || git -C "$SRC_DIR" merge --ff-only origin/master
else
    log "cloning ${REPO_URL} into ${SRC_DIR}"
    mkdir -p "$(dirname "$SRC_DIR")"
    git clone "$REPO_URL" "$SRC_DIR"
fi

# --- 4. build ----------------------------------------------------------------
log "building agent-sync (cli + daemon)"
mkdir -p "$BIN_DIR"
( cd "$SRC_DIR"
  CGO_ENABLED=0 go build -o "$BIN_DIR/agent-sync" ./cmd/cli
  CGO_ENABLED=0 go build -o "$BIN_DIR/agent-sync-daemon" ./cmd/daemon )

# --- 5. PATH -----------------------------------------------------------------
case ":$PATH:" in
    *":${BIN_DIR}:"*) ;;
    *) log "adding ${BIN_DIR} to PATH in ~/.bashrc"
       printf '\nexport PATH="%s:$PATH"\n' "$BIN_DIR" >> "$HOME/.bashrc"
       export PATH="${BIN_DIR}:$PATH" ;;
esac

# --- 6. OpenCode (optional runtime dependency) ------------------------------
if ! have opencode; then
    log "WARNING: opencode not found — sessions cannot be captured/resumed without it."
    log "         install the pinned version used for testing:  npm i -g opencode-ai@1.18.18"
fi

# --- 7. done -----------------------------------------------------------------
if [ "${1:-}" = "--run-help" ]; then
    log "post-install usage:"
    cat <<'EOF'
  agent-sync init [--repo <git-url>] [--device-alias <name>]   # configure + create sync repo
  agent-sync index                                             # scan [repoindex] roots once configured
  agent-sync send <session-id>                                 # capture one session now
  agent-sync receive                                           # pull + restore pending sessions
  agent-sync resume [--repo <code-repo>]                       # one-shot resume flow
  agent-sync pull                                              # fetch + fast-forward sync repo only
  agent-sync migrate-layout [--dry-run]                        # move legacy exports to revisions layout
  agent-sync revisions list [--project K] [--session ID]       # inspect stored revisions
  agent-sync conflicts [--json]                                # report same-session conflicts
  agent-sync recover <session-id> [--revision <prefix>]        # restore one revision of a conflict
  agent-sync-daemon                                            # real-time watcher (foreground)
EOF
fi

log "done: $(command -v agent-sync || echo "$BIN_DIR/agent-sync")"
log "re-run this script any time to update to latest master."
