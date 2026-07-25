// Ambient module declarations for pix extensions.
//
// MUST live OUTSIDE extensions/ — pi loads every `.ts` in extensions/ as an
// extension factory and crashes on declaration files. tsconfig `include` pulls
// this in for the type checker; pi never sees it.
//
// (Node globals like `process`/`require` come from @types/node + tsconfig — do
// not re-declare them here or you'll get duplicate-identifier errors.)

declare module "@earendil-works/pi-tui" {
  /** Visible (ANSI-stripped, wide-char-aware) column width of a string. */
  export function visibleWidth(str: string): number;
}
