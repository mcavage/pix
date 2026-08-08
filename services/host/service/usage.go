package service

// Usage is the `pix serve` help text. It lives with the capability rather than
// in a central help file, so a change to what serve does and a change to what
// serve says are the same edit.
const Description = `Run the long-running host services (execs the sibling pix-host serve):
memory (:11435, when enabled) and the monitor ingest listener (:11437, when
enabled) that the in-VM monitor tap POSTs to — 'pix monitor' is a pure offline
reader with no listener of its own. Positional args are the service list;
anything after -- is passed to pix-host serve unchanged.

You usually do NOT need to run this yourself: pix run / memory auto-start a
detached serve when its ports are down (logs in ~/.local/state/pix/serve.log).
Opt out with PIX_NO_AUTOSERVE=1 or 'pix config set host.autoserve false'.`
