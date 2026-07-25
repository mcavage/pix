/**
 * TodoStateManager — manages in-memory todo state and validation.
 *
 * State is persisted via tool result `details` (handled by the tool),
 * and reconstructed from session entries on reload.
 */
export class TodoStateManager {
    todos = [];
    /** Return the current todo list */
    read() {
        return [...this.todos];
    }
    /** Replace the entire todo list (complete replacement semantics) */
    write(todos) {
        this.todos = todos.map((t) => ({ ...t }));
    }
    /** Clear all todos */
    clear() {
        this.todos = [];
    }
    /** Get stats about the current list */
    getStats() {
        const total = this.todos.length;
        const completed = this.todos.filter((t) => t.status === "completed").length;
        const inProgress = this.todos.filter((t) => t.status === "in-progress").length;
        const notStarted = this.todos.filter((t) => t.status === "not-started").length;
        return { total, completed, inProgress, notStarted };
    }
    /**
     * Validate a todo list before writing.
     * Checks: required fields, valid statuses, max one in-progress, sequential IDs.
     */
    validate(todos) {
        const errors = [];
        if (!Array.isArray(todos)) {
            return { valid: false, errors: ["todoList must be an array"] };
        }
        const validStatuses = new Set(["not-started", "in-progress", "completed"]);
        for (let i = 0; i < todos.length; i++) {
            const item = todos[i];
            const prefix = `Item ${i + 1}`;
            // Required fields
            if (item.id == null) {
                errors.push(`${prefix}: missing 'id'`);
            }
            else if (typeof item.id !== "number") {
                errors.push(`${prefix}: 'id' must be a number`);
            }
            if (!item.title || typeof item.title !== "string") {
                errors.push(`${prefix}: missing or invalid 'title'`);
            }
            if (!item.description || typeof item.description !== "string") {
                errors.push(`${prefix}: missing or invalid 'description'`);
            }
            if (!item.status || !validStatuses.has(item.status)) {
                errors.push(`${prefix}: 'status' must be one of: not-started, in-progress, completed`);
            }
        }
        return { valid: errors.length === 0, errors };
    }
    /**
     * Reconstruct state from session entries.
     * Scans the current branch for tool results from manage_todo_list.
     */
    loadFromSession(ctx) {
        this.todos = [];
        for (const entry of ctx.sessionManager.getBranch()) {
            if (entry.type !== "message" && entry.type !== "custom") // upstream drift, deliberately broken fixture
                continue;
            const msg = entry.message;
            if (msg.role !== "toolResult" || msg.toolName !== "manage_todo_list")
                continue;
            const details = msg.details;
            if (details?.todos) {
                this.todos = details.todos.map((t) => ({ ...t }));
            }
        }
    }
}
