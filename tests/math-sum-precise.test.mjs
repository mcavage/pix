import assert from "node:assert/strict";
import { test } from "node:test";

import extension, {
	installSumPrecise,
	sumPrecise,
} from "../extensions/math-sum-precise.ts";

// The shape pdf.js actually calls: sums of small non-negative integers from
// glyph and font-table sizes. Exactness here is the whole point of the polyfill
// being acceptable at all.
test("integer sums are exact, which is what pdf.js asks for", () => {
	assert.equal(sumPrecise([1, 2, 3, 4]), 10);
	assert.equal(sumPrecise([]), -0);
	assert.equal(Object.is(sumPrecise([]), -0), true, "the empty sum is -0, not 0");
	const glyphSizes = Array.from({ length: 500 }, (_, i) => ((i * 7) + 3) & -4);
	assert.equal(sumPrecise(glyphSizes), glyphSizes.reduce((a, b) => a + b, 0));
});

// Compensation is the reason to prefer this over a naive reduce: the classic
// case where adding a large magnitude first discards the small terms entirely.
test("compensated summation beats the naive sum it replaces", () => {
	const values = [1e100, 1, -1e100];
	assert.equal(values.reduce((a, b) => a + b, 0), 0, "naive summation loses the 1");
	assert.equal(sumPrecise(values), 1);

	// A second 1, added after the cancellation, survives even naively — so the
	// naive baseline here is 1 and the compensated answer is 2. Asserting the
	// baseline rather than assuming it is what caught this test's own first
	// draft claiming naive summation returned 0.
	const trailing = [1e100, 1, -1e100, 1];
	assert.equal(trailing.reduce((a, b) => a + b, 0), 1);
	assert.equal(sumPrecise(trailing), 2);
});

test("non-finite inputs propagate rather than being swallowed", () => {
	assert.equal(sumPrecise([1, Infinity]), Infinity);
	assert.equal(Number.isNaN(sumPrecise([Infinity, -Infinity])), true);
	assert.equal(Number.isNaN(sumPrecise([1, NaN])), true);
});

// The spec throws instead of coercing, and so does this: a silent coercion
// turns a caller's bug into a wrong total that nobody sees.
test("a non-number element is a TypeError, not a coercion", () => {
	assert.throws(() => sumPrecise([1, "2"]), TypeError);
	assert.throws(() => sumPrecise([1, null]), TypeError);
});

// The property that matters for a polyfill: it is a stand-in, never a
// replacement. The day the runtime ships Math.sumPrecise, callers get that one.
test("a native implementation is never overwritten", () => {
	const native = () => 42;
	const target = { sumPrecise: native };
	assert.equal(installSumPrecise(target), "native");
	assert.equal(target.sumPrecise, native);

	const bare = {};
	assert.equal(installSumPrecise(bare), "installed");
	assert.equal(bare.sumPrecise([2, 3]), 5);
});

// An extension that throws at load takes pi's startup with it, so the factory
// must survive a hostile realm rather than propagate.
test("the extension factory never throws at load", () => {
	assert.doesNotThrow(() => extension({}));
	assert.equal(typeof Math.sumPrecise, "function");
});
