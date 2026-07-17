// External promptfoo assertion: run the model's shell one-liner and score 1 iff
// it prints the digits 1..5 (whitespace-insensitive). A mechanical grader, the
// kind of "build/test decides correctness" eval the router is meant to support.
//
// TRUSTED-GRADER caveat (same as v1's command scorer): this EXECUTES model
// output on the eval host. Point it only at trusted output; run it in a
// container/VM if it could ever grade untrusted code.

const { execSync } = require("node:child_process");

module.exports = (output) => {
	try {
		const cmd = String(output)
			.replace(/```[a-z]*\n?/gi, "")
			.replace(/```/g, "")
			.trim();
		const out = execSync(cmd, {
			timeout: 5000,
			encoding: "utf8",
			shell: "/bin/sh",
		});
		const pass = out.replace(/\s+/g, "") === "12345";
		return {
			pass,
			score: pass ? 1 : 0,
			reason: pass ? "printed 1..5" : `got ${JSON.stringify(out.slice(0, 40))}`,
		};
	} catch (e) {
		return { pass: false, score: 0, reason: `exec failed: ${String(e.message).slice(0, 80)}` };
	}
};
