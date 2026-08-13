// math-sum-precise.ts — supply Math.sumPrecise when the runtime lacks it.
//
// WHY THIS EXISTS. pi reads PDFs through `unpdf`, which bundles a pdf.js recent
// enough to call `Math.sumPrecise` while summing glyph and font-table sizes:
//
//     Math.sumPrecise(this.glyphs.map(e => e.getSize() + 3 & -4))
//     Math.sumPrecise(this.composites.map(e => e.getSize()))
//
// The sandbox runs Node v25.9, whose V8 does not expose that method (it is not
// behind --harmony either, checked). So every PDF read in a session raises
// `TypeError: Math.sumPrecise is not a function`, repeatedly, as a warning.
//
// WHY A POLYFILL AND NOT A PATCH. `unpdf` is installed at RUNTIME into
// ~/.pi/agent/npm, not baked into the image, so the build-time patching used
// for pi-manage-todo-list cannot reach it. An extension runs at pi startup,
// before any PDF is opened, and Math is one realm — so the definition is in
// place by the time pdf.js looks for it.
//
// It removes itself from relevance the day the runtime ships the real thing:
// a native Math.sumPrecise is never replaced.

// sumPrecise sums with Neumaier compensation.
//
// NOT bit-identical to the specified Math.sumPrecise, and the difference is
// worth stating: the real one is exactly rounded for any input, while this
// tracks a single compensation term. For what pdf.js does here — summing small
// non-negative integers — both are exact, and against ordinary float data
// compensation is far closer than the naive sum a caller would otherwise write
// by hand. Adversarially ordered floats can still differ in the last ulp.
export function sumPrecise(values: Iterable<number>): number {
	let sum = 0;
	let compensation = 0;
	let seen = 0;
	// Non-finite values are tallied rather than added. Feeding an infinity
	// through the compensation term poisons it — (sum - Infinity) + Infinity is
	// NaN — so a single Infinity in the input turned the whole sum into NaN,
	// where the real Math.sumPrecise answers Infinity. Caught by its own test.
	let sawNaN = false;
	let sawPosInf = false;
	let sawNegInf = false;
	for (const value of values) {
		// The spec throws for a non-number rather than coercing, and a silent
		// coercion here would turn a caller's bug into a wrong total.
		if (typeof value !== "number") {
			throw new TypeError("Math.sumPrecise: every value must be a Number");
		}
		seen++;
		if (Number.isNaN(value)) {
			sawNaN = true;
			continue;
		}
		if (value === Infinity) {
			sawPosInf = true;
			continue;
		}
		if (value === -Infinity) {
			sawNegInf = true;
			continue;
		}
		const next = sum + value;
		compensation +=
			Math.abs(sum) >= Math.abs(value) ? sum - next + value : value - next + sum;
		sum = next;
	}
	if (sawNaN || (sawPosInf && sawNegInf)) {
		return NaN;
	}
	if (sawPosInf) {
		return Infinity;
	}
	if (sawNegInf) {
		return -Infinity;
	}
	// The empty sum is -0, per the proposal. Returning 0 would be a different
	// value under Object.is, which is exactly the kind of detail a polyfill is
	// supposed to get right.
	if (seen === 0) {
		return -0;
	}
	return sum + compensation;
}

// installSumPrecise is separate from the factory so a test can assert the one
// property that matters for a polyfill: it defers to a real implementation.
export function installSumPrecise(target: any): "installed" | "native" {
	if (typeof target.sumPrecise === "function") {
		return "native";
	}
	Object.defineProperty(target, "sumPrecise", {
		value: sumPrecise,
		writable: true,
		configurable: true,
		enumerable: false,
	});
	return "installed";
}

export default function (_pi: any) {
	try {
		installSumPrecise(Math as any);
	} catch {
		// Best-effort; must not break the agent. An extension that throws at load
		// takes pi's startup with it, and a missing PDF sum is not worth that.
	}
}
