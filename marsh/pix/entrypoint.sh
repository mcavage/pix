#!/bin/sh
set -eu
umask 077
selected=${MARSH_SELECTED_HOME:-${HOME:-/home/agent}}
case "$selected" in
  /*) ;;
  *) echo 'pix: selected HOME must be absolute' >&2; exit 125 ;;
esac
export HOME=$selected
export PI_CODING_AGENT_DIR='~/.pix-agent/agent'
export PIX_PACKAGE_DIR=/opt/pix/package
agent="$HOME/.pix-agent/agent"
mkdir -p "$agent/themes" "$agent/context/output-styles"
# First-use defaults only. An existing settings file is never merged/replaced.
if [ ! -e "$agent/settings.json" ]; then
  cp -n /opt/pix/package/defaults/settings.json "$agent/settings.json"
fi
for theme in /opt/pix/package/themes/*.json; do
  name=${theme##*/}
  if [ ! -e "$agent/themes/$name" ] && [ ! -L "$agent/themes/$name" ]; then
    ln -s "$theme" "$agent/themes/$name" 2>/dev/null ||
      [ -e "$agent/themes/$name" ] || [ -L "$agent/themes/$name" ]
  fi
done
case "${SBX_CRED_ANTHROPIC_MODE:-none}" in
  apikey|oauth) export ANTHROPIC_API_KEY=proxy-managed ;;
esac
case "${SBX_CRED_OPENAI_MODE:-none}" in
  apikey) export OPENAI_API_KEY=proxy-managed ;;
esac
case "${SBX_CRED_GOOGLE_MODE:-none}" in
  apikey) export GEMINI_API_KEY=proxy-managed ;;
esac
if [ "${1:-}" = --acp ]; then
  shift
  export PI_ACP_PI_COMMAND=/usr/local/bin/marsh-pix
  exec node /opt/marsh/pi-gateway/node_modules/pi-acp/dist/index.js "$@"
fi
if [ -r /run/marsh/context.md ]; then
  set -- --append-system-prompt /run/marsh/context.md "$@"
fi
if [ "${PIX_UI:-on}" != off ]; then
  set -- -e /opt/pix/package/extensions/ui.mjs "$@"
fi
set -- -e /opt/pix/package/extensions/output-style.ts \
  --skill /opt/pix/package/skills \
  --append-system-prompt /opt/pix/package/instructions.md "$@"
# Bare `pi` is marsh's separate packaged command, not this image's executable.
exec /opt/marsh/pi-gateway/node_modules/.bin/pi --approve \
  -e /opt/marsh/pi-gateway/mcp-gateway.mjs "$@"
