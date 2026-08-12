#!/usr/bin/env node
// pi-manage-todo-list 0.4.0 documents `/todos` as a toggle, but the stock
// command only refreshes the widget. Add real toggle/hide/show controls, an
// Alt+T shortcut, and branch-persisted visibility without changing todo state.
// Keep `/todos clear` durable so resume and compaction never resurrect work the
// user explicitly cleared.

import fs from "node:fs";
import os from "node:os";
import path from "node:path";

// Release smoke installs into an isolated HOME and persists the exact dist
// path between workflow steps. Prefer that explicit target over rediscovery.
const root = process.env.TODO_DIST ||
	path.join(
		os.homedir(),
		".pi",
		"agent",
		"npm",
		"node_modules",
		"pi-manage-todo-list",
		"dist",
	);
const indexPath = path.join(root, "index.js");
const statePath = path.join(root, "state-manager.js");
const clearMarker = "pi-stack-todo-cleared";
const legacyClearMarker = "pix-todo-cleared";
const visibilityMarker = "pix-todo-widget-visibility";

function replaceExact(current, before, after, file) {
	if (!current.includes(before)) {
		throw new Error(`anchor not found in ${file}; refresh apply-todo-durable-clear.mjs`);
	}
	return current.replace(before, after);
}

function patchIndex() {
	const current = fs.readFileSync(indexPath, "utf8");
	if (current.includes(visibilityMarker)) return false;

	let next = current;
	next = replaceExact(
		next,
		`import { clearWidget, updateWidget } from "./ui/todo-widget.js";\n`,
		`import { clearWidget, updateWidget } from "./ui/todo-widget.js";\nconst VISIBILITY_ENTRY = "${visibilityMarker}";\n`,
		indexPath,
	);
	next = replaceExact(
		next,
		`    const state = new TodoStateManager();\n    /** Callback invoked after every write — updates the widget */\n    let currentCtx;\n    const onTodoUpdate = () => {\n        if (currentCtx) {\n            updateWidget(state, currentCtx);\n        }\n    };`,
		`    const state = new TodoStateManager();\n    /** Callback invoked after every write — updates the widget when visible. */\n    let currentCtx;\n    let widgetVisible = true;\n    const renderWidget = (ctx) => {\n        if (widgetVisible)\n            updateWidget(state, ctx);\n        else\n            clearWidget(ctx);\n    };\n    const onTodoUpdate = () => {\n        if (currentCtx)\n            renderWidget(currentCtx);\n    };\n    const loadWidgetVisibility = (ctx) => {\n        widgetVisible = true;\n        for (const entry of ctx.sessionManager.getBranch()) {\n            if (entry.type === "custom" &&\n                entry.customType === VISIBILITY_ENTRY &&\n                typeof entry.data === "object" &&\n                entry.data !== null &&\n                typeof entry.data.visible === "boolean") {\n                widgetVisible = entry.data.visible;\n            }\n        }\n    };\n    const setWidgetVisible = (visible, ctx) => {\n        currentCtx = ctx;\n        widgetVisible = visible;\n        pi.appendEntry(VISIBILITY_ENTRY, { visible });\n        renderWidget(ctx);\n        if (!visible) {\n            ctx.ui.notify("Todo list hidden. Tasks are still saved.", "info");\n            return;\n        }\n        const stats = state.getStats();\n        const message = stats.total === 0\n            ? "Todo list visibility enabled. No todos to display."\n            : \`Todo list shown. \${stats.completed}/\${stats.total} completed.\`;\n        ctx.ui.notify(message, "info");\n    };\n    const toggleWidget = (ctx) => setWidgetVisible(!widgetVisible, ctx);`,
		indexPath,
	);
	next = replaceExact(
		next,
		`        state.loadFromSession(ctx);\n        updateWidget(state, ctx);`,
		`        state.loadFromSession(ctx);\n        loadWidgetVisibility(ctx);\n        renderWidget(ctx);`,
		indexPath,
	);
	next = replaceExact(
		next,
		`        currentCtx = ctx;\n        updateWidget(state, ctx);\n    });\n    // --- Register the manage_todo_list tool ---`,
		`        currentCtx = ctx;\n        renderWidget(ctx);\n    });\n    // --- Register the manage_todo_list tool ---`,
		indexPath,
	);
	next = replaceExact(
		next,
		`    pi.registerCommand("todos", {\n        description: "Toggle todo list widget or clear todos (/todos clear)",\n        handler: async (args, ctx) => {\n            currentCtx = ctx;\n            if (args?.trim().toLowerCase() === "clear") {\n                state.clear();\n                clearWidget(ctx);\n                ctx.ui.notify("Todo list cleared.", "info");\n                return;\n            }\n            // Toggle: if todos exist, update widget; if empty, notify\n            const todos = state.read();\n            if (todos.length === 0) {\n                ctx.ui.notify("No todos. The LLM will create them when working on complex tasks.", "info");\n            }\n            else {\n                updateWidget(state, ctx);\n                ctx.ui.notify(\`\${state.getStats().completed}/\${state.getStats().total} todos completed.\`, "info");\n            }\n        },\n    });\n}`,
		`    pi.registerCommand("todos", {\n        description: "Toggle, hide, show, or clear the todo list widget (/todos [hide|show|clear])",\n        handler: async (args, ctx) => {\n            currentCtx = ctx;\n            const action = args?.trim().toLowerCase() ?? "";\n            if (action === "clear") {\n                state.clear();\n                pi.appendEntry("${clearMarker}", {});\n                clearWidget(ctx);\n                ctx.ui.notify("Todo list cleared.", "info");\n                return;\n            }\n            if (action === "hide") {\n                setWidgetVisible(false, ctx);\n                return;\n            }\n            if (action === "show") {\n                setWidgetVisible(true, ctx);\n                return;\n            }\n            if (action === "") {\n                toggleWidget(ctx);\n                return;\n            }\n            ctx.ui.notify("Usage: /todos [hide|show|clear]", "warning");\n        },\n    });\n    pi.registerShortcut("alt+t", {\n        description: "Toggle todo list widget",\n        handler: async (ctx) => toggleWidget(ctx),\n    });\n}`,
		indexPath,
	);

	fs.writeFileSync(indexPath, next);
	return true;
}

function patchState() {
	const current = fs.readFileSync(statePath, "utf8");
	const after = `        for (const entry of ctx.sessionManager.getBranch()) {\n            if (entry.type === "custom" &&\n                (entry.customType === "${clearMarker}" || entry.customType === "${legacyClearMarker}")) {\n                this.todos = [];\n                continue;\n            }\n            if (entry.type !== "message")\n                continue;`;
	if (current.includes(after)) return false;
	const before = `        for (const entry of ctx.sessionManager.getBranch()) {\n            if (entry.type !== "message")\n                continue;`;
	fs.writeFileSync(statePath, replaceExact(current, before, after, statePath));
	return true;
}

const indexChanged = patchIndex();
const stateChanged = patchState();

console.log(
	indexChanged || stateChanged
		? "[apply-todo-durable-clear] patched"
		: "[apply-todo-durable-clear] already patched",
);
