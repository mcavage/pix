# Marsh-native Pix

This directory is the new, user-approved marsh-native Pix Kit/package. The
repository root still contains the legacy launcher; do not rewrite or retire it
as part of this branch. Its launcher/PIX_HOME rules are historical for this new
package, not a requirement to rebuild those services here.

- Use upstream Pi and its supported extension APIs. No Pi patches or custom
  subagent runner, scheduler, model gateway, Docker/SBX controller, or memory
  service in this package. Orchestration belongs in skills using marsh commands.
- Keep the Kit source outside mounted projects/guest homes when installing.
- Public inputs are explicitly selected Git-tracked generic skills/roles/themes
  and this directory's source. Never copy a personal ~/.marsh or ~/.pix tree,
  private skills, MCP declarations, credentials, sessions, or business evidence.
- Build/test generated snapshots in a separate temporary directory. Preserve
  existing personal configuration. Do not activate the user's Kit or start their
  MCP connections as part of tests. No authentication/model calls without approval.
- Test the real upstream-Pi entrypoint, UI/resource loading, stock-Pi separation,
  and config preservation. Discovery is not authenticated-provider proof.
- Keep source pins explicit. The Pi base follows `marsh-pi:latest`, but every
  build names one digest (`kit_inputs.py`), recorded in the image. The
  inherited base's legal/verification receipts do not certify a derived image;
  publication (`.github/workflows/marsh-pix.yml`) remains a separate gate.

Run `python3 -m unittest discover -s marsh/tests -v` for preparation/privacy
checks. `python3 marsh/tests/runtime.py` builds and checks an isolated snapshot
with Docker, without SBX, MCP or model calls. No commit/push unless requested.
