// description.go — the prose above `pix setup`'s GENERATED usage. It lives
// in this package (rather than in cmd/pix beside the command struct) so the
// workflow that performs setup owns the sentence describing it; the flag
// list is not here, since the command struct's tags are the flag list.
package provision

// Description is what `pix setup` guarantees and how a repeat behaves.
const Description = `Prepare Pix for your first session: check prerequisites, install its runtime,
choose a model, and connect the accounts your environment uses.

Use --env NAME to set up an environment you added with pix env add.
Setup keeps completed connections and can be rerun after an interruption.
Add --verbose for technical details. When setup finishes, run pix to begin.
`
