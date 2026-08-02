package knowledge

// Usage text lives with the capability that owns the verb, so changing what
// knowledge does and changing what knowledge says are the same edit. It was in
// a central help.go, which is how the two drift.

const Usage = `usage: pix knowledge <init|use|ls|query|sync|remote> [args]

  init [DIR]                     scaffold + wire a global OKF bundle
  use <path|url>                 point the global KB at a bundle (path made
                                 absolute; not checked for existence/OKF)
  use --project <path|url> [--dir D]   write a per-repo .pix/knowledge pointer
  ls [--json]                    list configured bundles + daemon health
  query <text...> [--limit N] [--json]   search the knowledge daemon (:11436)
  sync [-m MSG] [--bundle D] [--allow-main]   commit + push the bundle
  remote [set <url>] [--bundle D]   show or set the bundle's git remote
`

const InitUsage = `usage: pix knowledge init [DIR]

Scaffold a spec-correct OKF bundle (default <config-dir>/knowledge), git-init it,
and wire it into config (services += knowledge, knowledge_bundles += DIR).
Idempotent: never clobbers an existing bundle.
`
