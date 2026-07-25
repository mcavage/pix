// pix — auto-dismiss the todo widget once every item is complete.
//
// The `pi-manage-todo-list` package pins a widget via ctx.ui.setWidget("todo-list", …)
// and RE-RENDERS it on every `turn_end` from its own (private) state. It only ever
// clears on the explicit `/todos clear` command, so a 100%-complete list stays
// pinned forever, taking up space with nothing left to track.
//
// This companion extension watches the manage_todo_list writes; when a write leaves
// EVERY todo completed (and the list is non-empty), it lets the final ✓ state linger
// briefly (so you see it land) and then clears the widget — and keeps it cleared on
// subsequent turns (re-clearing after the package re-renders) until a new, not-yet-
// complete list is written. A fresh incomplete list shows normally again.
//
// We clear the WIDGET only (we can't reach the package's private state), so we must
// re-suppress each turn while the completed list persists; the clear is scheduled on
// a timer so it runs AFTER the package's own turn_end render, regardless of hook order.
//
// Config: PI_TODO_AUTOCLEAR=0 disables it; PI_TODO_AUTOCLEAR_MS sets the linger before
// the first dismiss (default 4000ms). Fully defensive: never throws at load, never
// breaks the agent.

const WIDGET_ID = "todo-list"; // must match pi-manage-todo-list's ui/todo-widget.js
const TOOL = "manage_todo_list";

const safe = <T>(fn: () => T): T | undefined => {
	try {
		return fn();
	} catch {
		return undefined; /* best-effort; must not break the agent */
	}
};

export default function (pi: any) {
	const env = (globalThis as any).process?.env ?? {};
	if (env.PI_TODO_AUTOCLEAR === "0") return; // opt-out

	const linger = (() => {
		const n = Number(env.PI_TODO_AUTOCLEAR_MS);
		return Number.isFinite(n) && n >= 0 ? n : 4000;
	})();

	let ctx: any;
	let allDone = false; // last write left a non-empty list fully completed
	let suppress = false; // we've passed the linger and are actively hiding it
	let timer: any = null;

	const clearTimer = () => {
		if (timer) {
			safe(() => clearTimeout(timer));
			timer = null;
		}
	};

	const toolName = (e: any): string =>
		e?.toolName ?? e?.name ?? e?.tool?.name ?? e?.tool?.toolName ?? "";

	// Writes carry the full todoList; reads don't. Probe the shapes pi/tools use.
	const todoListOf = (e: any): any[] | null => {
		let a =
			e?.arguments ??
			e?.args ??
			e?.input ??
			e?.params ??
			e?.tool?.arguments ??
			e?.tool?.args;
		if (typeof a === "string") a = safe(() => JSON.parse(a));
		const list = a?.todoList ?? a?.todos;
		return Array.isArray(list) ? list : null;
	};

	const onWrite = (e: any) =>
		safe(() => {
			if (toolName(e) !== TOOL) return;
			const list = todoListOf(e);
			if (!list) return; // a read, or a shape we couldn't parse — ignore
			clearTimer();
			const total = list.length;
			const completed = list.filter((t: any) => t?.status === "completed").length;
			if (total > 0 && completed === total) {
				allDone = true; // schedule dismiss on turn_end
			} else {
				// an active or freshly-incomplete list: let it show normally again
				allDone = false;
				suppress = false;
			}
		});

	// tool_call and tool_execution_start are the two hooks that may carry args;
	// whichever fires first with a parseable todoList wins (clearTimer dedupes).
	pi.on?.("tool_call", onWrite);
	pi.on?.("tool_execution_start", onWrite);

	pi.on?.("turn_start", (_e: any, c: any) => {
		if (c) ctx = c;
	});

	pi.on?.("turn_end", (_e: any, c: any) =>
		safe(() => {
			if (c) ctx = c;
			if (!allDone) return; // nothing complete-and-idle to hide
			if (!ctx?.ui?.setWidget) return;
			// The package re-renders the (completed) widget in its own turn_end. Schedule
			// the clear on a timer so it runs after that render, whatever the hook order.
			clearTimer();
			const target = ctx;
			const delay = suppress ? 0 : linger; // linger only on the first dismiss
			timer = setTimeout(
				() =>
					safe(() => {
						suppress = true;
						target.ui.setWidget(WIDGET_ID, undefined);
					}),
				delay,
			);
			safe(() => timer?.unref?.()); // don't keep the event loop alive
		}),
	);

	pi.on?.("session_shutdown", () =>
		safe(() => {
			clearTimer();
			ctx?.ui?.setWidget?.(WIDGET_ID, undefined);
		}),
	);
}
