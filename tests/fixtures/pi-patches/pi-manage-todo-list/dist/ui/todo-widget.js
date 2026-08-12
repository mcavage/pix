export function updateWidget(state, ctx) {
  if (state.read().length === 0) {
    ctx.ui.setWidget("todo-list", undefined);
    return;
  }
  ctx.ui.setWidget("todo-list", { todos: state.read() });
}

export function clearWidget(ctx) {
  ctx.ui.setWidget("todo-list", undefined);
}
