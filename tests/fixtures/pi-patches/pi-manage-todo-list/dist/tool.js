export function createManageTodoListTool(state, onTodoUpdate) {
  return {
    name: "manage_todo_list",
    read: () => state.read(),
    write: (todos) => {
      state.write(todos);
      onTodoUpdate();
    },
  };
}
