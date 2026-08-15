package main

// redrive_findings3_test.go — rereview redrive findings 3/4 + DX JSON
// finding 2:
//
//  3: status must read each discovered sandbox's receipt even when the
//     current cfg/pack MCP intent is empty — a receipt-only transient name
//     (a `run --pack` mix-in, or a since-switched pack's historical
//     integration) must still surface as a per-sandbox row (human + --json),
//     while the host-global summary (which only ever reflects current
//     cfg/pack intent) correctly stays empty.
//  4: doctor tracks sbx-on-PATH separately from a successful `sbx secret
//     ls`. When sbx is on PATH but the secret probe fails, `sbx mcp ls` must
//     still be attempted — the MCP/gog groups get the on-path truth (not the
//     secret-probe's success/failure) as their "sbx present" signal, so they
//     can render ready/todo instead of falsely degrading to "sbx
//     unavailable". Providers stay unverifiable (that probe genuinely
//     failed); PATH-absent still reads absent everywhere.
//  DX JSON #2: verdict=ready must mean verified working. A note-only check
//     must carry a TRUTHFUL verdict (ready for a confirmed positive fact,
//     unverifiable for "cannot verify"/"not configured"/anything else) —
//     result() must not blanket-override to ready just because note is set.

// --- finding 3: per-sandbox MCP rows with an empty current intent ----------

// --- DX JSON finding 2: verdict=ready must mean verified working -----------
