import fs from "node:fs";

const APP_NAME = "pi";
function quoteIfNeeded(value) {
    if (value.length > 0 && !/[^a-zA-Z0-9_\-./~:@]/.test(value)) return value;
    return `'${value.replace(/'/g, `'\\''`)}'`;
}

export function formatResumeCommand(sessionManager) {
    if (!process.stdout.isTTY)
        return undefined;
    if (!sessionManager.isPersisted())
        return undefined;
    const sessionFile = sessionManager.getSessionFile();
    if (!sessionFile || !fs.existsSync(sessionFile))
        return undefined;
    const args = [APP_NAME];
    if (!sessionManager.usesDefaultSessionDir()) {
        args.push("--session-dir", quoteIfNeeded(sessionManager.getSessionDir()));
    }
    args.push("--session", sessionManager.getSessionId());
    return args.join(" ");
}
