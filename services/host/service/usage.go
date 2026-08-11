package service

// Usage is the `pix serve` help text. It lives with the capability rather than
// in a central help file, so a change to what serve does and a change to what
// serve says are the same edit.
const Description = `Run the long-running host services (execs the sibling pix-host serve):
memory (:11435, when enabled), plus any daemons the active pack declares in
[[services]] — those are supervised here, so a wedged one is replaced rather
than left holding its port. Positional args are the service list;
anything after -- is passed to pix-host serve unchanged.

You usually do NOT need to run this yourself: pix run / memory auto-start a
detached serve when its ports are down (logs in ~/.local/state/pix/serve.log).
Opt out with PIX_NO_AUTOSERVE=1 or 'pix config set host.autoserve false'.`
