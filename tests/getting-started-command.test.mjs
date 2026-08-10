import assert from "node:assert";
import { test } from "node:test";
import helpExtension from "../extensions/help.ts";

function loadCommands() {
	const commands = new Map();
	helpExtension({
		registerCommand(name, config) { commands.set(name, config); },
		on() {},
	});
	return commands;
}

test("/getting-started is a real registered command, not only a docs promise", () => {
	const commands = loadCommands();
	assert.ok(commands.has("getting-started"));
	assert.match(commands.get("getting-started").description, /first-session tour/i);
});

test("/getting-started renders the core agent workflow without triggering a model turn", async () => {
	const command = loadCommands().get("getting-started");
	let notification;
	await command.handler("", {
		ui: { notify(text, level) { notification = { text, level }; } },
	});
	assert.equal(notification.level, "info");
	for (const expected of ["/help", "/skill:<name>", "/model", "/status", "/recall", "pix task"]) {
		assert.match(notification.text, new RegExp(expected.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
	}
});
