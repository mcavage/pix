// Test stub for @earendil-works/pi-tui (see tests/stub-loader.mjs).
export class Container {
	children = [];
	addChild(child) { this.children.push(child); }
	render(width) { return this.children.flatMap((child) => child.render?.(width) ?? []); }
	invalidate() { for (const child of this.children) child.invalidate?.(); }
}
export class Text {
	constructor(text = "") { this.text = text; }
	render() { return this.text.split("\n"); }
	invalidate() {}
}
export class SelectList {
	constructor(items) { this.items = items; this.index = 0; }
	setSelectedIndex(index) { this.index = index; }
	setFilter() {}
	getSelectedItem() { return this.items[this.index] ?? null; }
	invalidate() {}
	render() { return []; }
	handleInput(data) {
		if (data === "down" || data === "\u001b[B") {
			this.index = Math.min(this.items.length - 1, this.index + 1);
			this.onSelectionChange?.(this.items[this.index]);
		} else if (data === "enter" || data === "\r") this.onSelect?.(this.items[this.index]);
		else if (data === "escape" || data === "\u001b") this.onCancel?.();
	}
}
export class Spacer {
	constructor() {}
}
export class Markdown {
	constructor() {}
}
