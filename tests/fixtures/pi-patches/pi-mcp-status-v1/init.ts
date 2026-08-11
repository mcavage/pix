  const connectedCount = results.filter(r => r.connection).length;
  const failedCount = results.filter(r => r.error).length;
  // pix MCP problems-only status: a healthy startup needs no toast.
  if (ui && connectedCount > 0 && (failedCount > 0 || state.config.settings?.mcpFooterStatus !== "problems")) {
    const totalTools = totalToolCount(state);
    const msg = failedCount > 0
      ? `MCP: ${connectedCount}/${startupServers.length} servers connected (${totalTools} tools)`
      : `MCP: ${connectedCount} servers connected (${totalTools} tools)`;
    ui.notify(msg, "info");
  }

export function updateStatusBar(state: McpExtensionState): void {
  publishMcpStatusSnapshot(state);
  const ui = state.ui;
  if (!ui) return;
  const entries = Object.entries(state.config.mcpServers);
  const disabledCount = entries.filter(([, definition]) => isServerDisabled(definition)).length;
  const enabledCount = entries.length - disabledCount;
  if (entries.length === 0) {
    ui.setStatus("mcp", undefined);
    return;
  }
  const connectedCount = [...state.manager.getAllConnections()].filter(([name, connection]) => {
    const definition = state.config.mcpServers[name];
    return connection.status === "connected" && definition !== undefined && !isServerDisabled(definition);
  }).length;
  const footerStatus = state.config.settings?.mcpFooterStatus ?? "full";
  if (footerStatus === "off" || (footerStatus === "problems" && connectedCount === enabledCount)) {
    ui.setStatus("mcp", undefined);
    return;
  }

  const disconnectedCount = enabledCount - connectedCount;
  let status = footerStatus === "compact"
    ? `MCP ${connectedCount}/${enabledCount}`
    : footerStatus === "problems"
      ? `${disconnectedCount} ${disconnectedCount === 1 ? "server" : "servers"} disconnected`
      : `${enabledCount} ${enabledCount === 1 ? "server" : "servers"} enabled`;
  if (footerStatus === "full") {
    if (connectedCount > 0) status += ` (${connectedCount} connected)`;
    if (disabledCount > 0) status += ` (${disabledCount} disabled)`;
  }
  const formattedStatus = footerStatus === "compact" ? status : formatMcpStatus(state.config, status);
  if (formattedStatus === undefined) {
    ui.setStatus("mcp", undefined);
    return;
  }
  ui.setStatus("mcp", ui.theme ? ui.theme.fg("accent", formattedStatus) : formattedStatus);
}
