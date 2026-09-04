// Package launcher is the running binary's own identity: its version. The
// two-binary pairing this package used to verify (a sibling `pix-host`
// process) was deleted in the Pix v2 cutover — there is one compiled host
// binary now (docs/design/pix-v2-architecture.md §14, AC-16).
package launcher

// Version is stamped at build time via -ldflags. An unstamped build reports
// "dev" and tracks the kit's main branch.
var Version = "dev"
