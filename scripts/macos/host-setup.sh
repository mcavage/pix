#!/usr/bin/env bash
# pi-stack macOS host setup — guided, one step at a time.
#
# This is the runnable version of docs/HANDOFF-repo-less-plugin-upgrade.md.
# It does the MECHANICAL steps and STOPS at every credential or verify gate so
# YOU stay in control. It never runs an agent, never force-anything, and each
# step is skippable + re-runnable (idempotent). Run it from a checkout:
#
#     bash scripts/macos/host-setup.sh
#
# Nothing here is full-auto: the whole point of pi-stack is that full-auto lives
# in the disposable sandbox, not on your Mac.
set -u

# --- resolve repo root from this script's location -----------------------------
SRC="${BASH_SOURCE[0]}"; while [ -h "$SRC" ]; do D="$(cd -P "$(dirname "$SRC")" && pwd)"; SRC="$(readlink "$SRC")"; [[ "$SRC" != /* ]] && SRC="$D/$SRC"; done
ROOT="$(cd -P "$(dirname "$SRC")/../.." && pwd)"
cd "$ROOT"

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }

# ask "prompt" -> returns 0 for yes/run, 1 for skip. Default skip on empty.
ask() { local a; printf '\n\033[1m%s\033[0m [y/N] ' "$1"; read -r a || a=n; case "$a" in y|Y|yes) return 0;; *) return 1;; esac; }
pause() { printf '\n%s ' "${1:-Press Enter to continue…}"; read -r _ || true; }

[ "$(uname)" = "Darwin" ] || { warn "This script targets macOS; you're on $(uname). Continuing, but launchd steps won't apply."; }

bold "pi-stack host setup — $ROOT"
echo "Walks the host steps in order. Skip any with N. Full guide: docs/HANDOFF-repo-less-plugin-upgrade.md"

# --- Step 0: preflight (read-only) ---------------------------------------------
bold "\n[0] Preflight — checking tools (read-only)"
for t in docker sbx gh go make; do
  if command -v "$t" >/dev/null 2>&1; then ok "$t: $(command -v "$t")"; else warn "$t: MISSING — install before the steps that need it"; fi
done
command -v ollama >/dev/null 2>&1 && ok "ollama: present (memory capture/recall)" || info "ollama: absent (optional — memory degrades to keyword)"

# --- Step 1: bake the image (needs DHI login; heavy) ---------------------------
bold "\n[1] Bake the image for sandboxes (make load)"
info "Rebuilds the sbx image so new skills/extensions/host land in fresh sandboxes."
info "Needs 'docker login dhi.io' (DHI-entitled account) and ~1GB. Or skip and let CI publish on PR merge."
if ask "Run 'make load' now?"; then
  docker login dhi.io || warn "docker login failed — 'make load' will likely fail without DHI access"
  make load || warn "make load failed — see output above"
else info "Skipped. (CI bakes + publishes on merge; or run 'make load' later.)"; fi

# --- Step 2: HOST-VERIFY #1 — sbx honors the git-ref kit pin --------------------
bold "\n[2] VERIFY #1 — does your sbx honor the '#ref=' kit pin?"
info "The launcher pins the kit to its release via #ref=v<version>. Confirm your sbx build accepts it."
if ask "Run the reftest sandbox now?"; then
  if sbx run pi-stack --name pi-stack-reftest --kit "git+https://github.com/mcavage/pi-stack.git#ref=main&dir=pi-kit" . ; then
    ok "Booted → #ref works; version-coupled 'pi-stack run' is good."
  else
    warn "sbx rejected #ref. Fallback: pin the image via --template (see cmd/pi-stack/sbxargs.go TODO)."
  fi
  sbx rm -f pi-stack-reftest >/dev/null 2>&1 || true
else info "Skipped — but verify before relying on 'pi-stack run' version pinning."; fi

# --- Step 3: build + install the launcher --------------------------------------
bold "\n[3] Install the launcher + host binary"
if ask "Build (make launcher) and install (make install) now?"; then
  make launcher && ok "built out/pi-stack" || warn "make launcher failed"
  make install && ok "symlinked bin/pi-stack onto PATH (~/.local/bin)" || warn "make install failed"
  info "Ensure ~/.local/bin is on your PATH. (Or, after a release: curl -fsSL .../install.sh | sh)"
else info "Skipped."; fi

# --- Step 4: launchd agent for host services -----------------------------------
bold "\n[4] Run host services as a launchd agent (memory :11435, gws :11441)"
PLIST="$HOME/Library/LaunchAgents/com.pi-stack.serve.plist"
info "Installs $PLIST (RunAtLoad + KeepAlive), pointed at your pi-stack-host + \$HOME."
if ask "Install and load the launchd agent now?"; then
  mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
  cp "$ROOT/scripts/macos/com.pi-stack.serve.plist" "$PLIST"
  # point the plist at this user + the installed binary
  BIN="$HOME/.local/bin/pi-stack-host"; [ -x "$BIN" ] || BIN="$ROOT/out/pi-stack-host"
  sed -i '' "s#/Users/CHANGEME/.local/bin/pi-stack-host#$BIN#g; s#/Users/CHANGEME#$HOME#g" "$PLIST"
  launchctl unload "$PLIST" >/dev/null 2>&1 || true
  if launchctl load -w "$PLIST"; then ok "agent loaded — logs at ~/Library/Logs/pi-stack-serve.{out,err}.log"; else warn "launchctl load failed — check the plist paths"; fi
  info "Stop later:  launchctl unload $PLIST"
else info "Skipped. Run services in a terminal instead:  pi-stack serve"; fi

# --- Step 5: wire your accounts (interactive) ----------------------------------
bold "\n[5] Wire your accounts — setup + doctor"
info "setup seeds ~/.config/pi-stack/config.toml and prompts for missing secrets/ollama/gws/mcp."
if ask "Run 'pi-stack setup' now?"; then pi-stack setup || warn "pi-stack setup returned nonzero"; else info "Skipped."; fi
if ask "Run 'pi-stack doctor' to confirm?"; then pi-stack doctor || true; else info "Skipped."; fi

# --- Step 6: HOST-VERIFY #2 — broker bearer reaches the VM ---------------------
bold "\n[6] VERIFY #2 — does the broker bearer (GWS_TOKEN_AUTH) reach the sandbox?"
info "kit-spec v1 has no dynamic host-env->VM passthrough, so this is the one open host decision."
info "The launcher sets it in the sbx process env (off argv). Confirm it lands in the VM:"
if ask "Launch a test sandbox to check?"; then
  info "Starting a sandbox. INSIDE it, run:   echo \"\${GWS_TOKEN_AUTH:-EMPTY}\""
  info " - non-empty  -> bearer enforced end-to-end, you're done."
  info " - EMPTY      -> use 'sbx secret' or launcher-side 'sbx run --env GWS_TOKEN_AUTH' (name only)."
  info "   Until wired, gws still works (falls back to unauthenticated, logs a warning). Not broken, just not enforced."
  pause "Press Enter to launch the test sandbox (exit it when done, then Enter here again)…"
  pi-stack run --name pi-stack-bearertest . || warn "launch failed"
  sbx rm -f pi-stack-bearertest >/dev/null 2>&1 || true
else info "Skipped — verify before depending on bearer enforcement."; fi

bold "\n[7] Optional: private overlay for work"
info "snow/warehouse broker + private memory/OKF live in your overlay peer repo (not this one)."
info "Wire them as SHA-pinned [plugins.*] overrides in ~/.config/pi-stack/config.toml — docs/OVERLAY.md §4."

bold "\nDone."
echo "Merge the PR when you're happy; CI tags v0.0.<run>, builds the image, and uploads release binaries."
