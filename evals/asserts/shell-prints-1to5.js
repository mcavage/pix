// EXAMPLE, OPT-IN, NOT wired into any default suite. External promptfoo
// assertion: run the model's shell one-liner and score 1 iff it prints 1..5. A
// mechanical grader, the kind of "build/test decides correctness" eval the
// router supports.
//
// DANGER: this EXECUTES model output on the eval host with your full
// environment. The default code.yaml deliberately uses a NON-executing regex
// check instead. Only reference this from a suite when the output is TRUSTED,
// and run it inside a disposable container/VM (no network, no secrets,
// unprivileged UID, read-only workspace) before grading anything untrusted.
// Wire it with:  - type: javascript\n    value: file://../asserts/shell-prints-1to5.js

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
