#!/usr/bin/env node
// pix appends a [pix-trusted-host-state] block to its own generated onboarding
// prompt — that block is the ENTIRE mechanism by which trusted host facts reach
// the fenced in-sandbox agent, so it has to be in the message. But it is a
// message, so pi renders it, and the first thing a new user sees is ~400
// characters of wrapped JSON above the greeting.
//
// Mark flagged it twice, the second time as "again this goofy json". It also
// actively misinforms: `"openai":false,"google":false` reads as "those providers
// are broken" when the truth is that this host needs no provider keys at all,
// which the very next field says.
//
// So: strip it from the DISPLAY text only. The agent still receives it — this
// patch touches `textContent`, which pi uses for the chat component and the
// editor history, never message.content, which is what goes to the model.
// Keeping it out of the editor history is a small bonus: nobody wants to arrow
// up into a JSON blob.
//
// FATAL on a missing anchor, deliberately. This is cosmetic, so a warn-and-
// continue would be defensible — except the failure mode is the blob silently
// coming back on a pi upgrade, which is exactly the thing being fixed. A build
// failure is cheaper than a user reporting it a third time.

import fs from "node:fs";

const target = process.env.PI_INTERACTIVE_MODE ||
	"/usr/local/share/npm-global/lib/node_modules/@earendil-works/pi-coding-agent/dist/modes/interactive/interactive-mode.js";
const marker = "pix hide trusted host state";
const before = `            case "user": {
                const textContent = this.getUserMessageText(message);
                if (textContent) {`;
const after = `            case "user": {
                let textContent = this.getUserMessageText(message);
                // ${marker}: the block is context for the agent, not for the
                // reader. Display and editor history only — message.content is
                // untouched, so the model still gets every field.
                // Two preconditions, both load-bearing. pix injects the block into
                // its own generated prompt and nothing else, so requiring that
                // prefix is what stops this eating the text of a user who types
                // the tag while ASKING about it. And a block with no closing tag
                // is left alone: truncating there would hide a malformed payload
                // instead of showing it.
                if (textContent && textContent.startsWith("[pix-generated:")) {
                    const pixOpen = "[pix-trusted-host-state]";
                    const pixClose = "[/pix-trusted-host-state]";
                    const pixAt = textContent.indexOf(pixOpen);
                    const pixEnd = pixAt >= 0 ? textContent.indexOf(pixClose, pixAt) : -1;
                    if (pixEnd >= 0) {
                        textContent = (textContent.slice(0, pixAt) +
                            textContent.slice(pixEnd + pixClose.length)).trimEnd();
                    }
                }
                if (textContent) {`;

const source = fs.readFileSync(target, "utf8");
if (source.includes(marker)) {
	console.log("[apply-hide-host-state] already patched");
} else {
	if (!source.includes(before)) {
		throw new Error(`anchor not found in ${target}; refresh apply-hide-host-state.mjs`);
	}
	fs.writeFileSync(target, source.replace(before, after));
	console.log("[apply-hide-host-state] patched");
}
