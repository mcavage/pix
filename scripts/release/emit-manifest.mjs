#!/usr/bin/env node
// Emits the release manifest that binds ONE Pix version to every published
// artifact digest (docs/design/pix-v2-architecture.md §3):
//
//   "pix-agent and pix-memory have independent Dockerfiles and immutable
//    digests. A release manifest binds one Pix version to the binary, both
//    image digests, the runtime archive digest, and the kit revision. Setup
//    and doctor read that one manifest; there is no separately maintained
//    version map."
//
// This script does not build or publish anything: it only binds identities
// that were already produced elsewhere (docker build/push, the runtime
// archive, git) into one JSON document. It never fabricates a digest — every
// field is either read from a real file or is passed in explicitly, and a
// missing required input fails the command rather than emitting a manifest
// that lies about what shipped.
//
// Usage:
//   node scripts/release/emit-manifest.mjs \
//     --version 0.1.71 \
//     --agent-digest sha256:... \
//     --memory-digest sha256:... \
//     --runtime-archive out/pix-runtime-0.1.71.tar.gz \
//     [--kit-revision <git-sha> | default: git log -1 -- pi-kit] \
//     [--write out/release-manifest.json]
//
// Exported functions are pure (no fs/process side effects) so tests can
// exercise the shape without a real git checkout or built archive.

import { createHash } from "node:crypto";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { execFileSync } from "node:child_process";

const DIGEST_RE = /^sha256:[0-9a-f]{64}$/;

export function validateDigest(name, value) {
	if (typeof value !== "string" || !DIGEST_RE.test(value)) {
		throw new Error(`${name} must be an immutable "sha256:<64 hex>" digest, got: ${JSON.stringify(value)}`);
	}
}

export function sha256File(path) {
	const buf = readFileSync(path);
	return `sha256:${createHash("sha256").update(buf).digest("hex")}`;
}

// buildManifest({ version, agentDigest, memoryDigest, runtimeDigest, kitRevision })
// -> the plain object written as JSON. Every field is required: a manifest
// with a missing binding is not a partial manifest, it is a wrong one — the
// whole point is that setup/doctor can trust ONE document instead of
// re-deriving identity from a version string.
// versionRE mirrors services/host/release/release.go's versionRE exactly: the
// safe filename/release-path grammar (conservative, semver-compatible,
// leading alnum, no slash/backslash/control byte). A CI release always
// passes a clean X.Y.Z; a LOCAL build (`make bundle`/`make release-manifest`)
// passes the SAME derived identity the launcher binary stamps
// (scripts/release/derive-build-version.sh: X.Y.(Z+1)-beta.<sha7>[.dirty.<12hex>]),
// so the manifest's "version" field never disagrees with the binary reading it.
const versionRE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

export function buildManifest({ version, agentDigest, memoryDigest, runtimeDigest, kitRevision, generatedAt }) {
	const v = String(version);
	if (!versionRE.test(v) || v.includes("..")) {
		throw new Error(`version must match the safe release grammar [A-Za-z0-9][A-Za-z0-9._-]* with no ".." segment, got: ${JSON.stringify(version)}`);
	}
	validateDigest("agentDigest", agentDigest);
	validateDigest("memoryDigest", memoryDigest);
	validateDigest("runtimeDigest", runtimeDigest);
	if (!kitRevision || typeof kitRevision !== "string" || !/^[0-9a-f]{7,40}$/.test(kitRevision)) {
		throw new Error(`kitRevision must be a git commit SHA (short or full), got: ${JSON.stringify(kitRevision)}`);
	}
	return {
		schemaVersion: 1,
		version: String(version),
		artifacts: {
			"pix-agent": { digest: agentDigest },
			"pix-memory": { digest: memoryDigest },
			runtime: { digest: runtimeDigest },
		},
		kitRevision,
		generatedAt: generatedAt ?? new Date().toISOString(),
	};
}

function parseArgs(argv) {
	const out = {};
	for (let i = 0; i < argv.length; i++) {
		const a = argv[i];
		if (!a.startsWith("--")) continue;
		const key = a.slice(2);
		out[key] = argv[++i];
	}
	return out;
}

function defaultKitRevision(repoRoot) {
	return execFileSync("git", ["log", "-1", "--format=%H", "--", "pi-kit"], {
		cwd: repoRoot,
		encoding: "utf8",
	}).trim();
}

function main() {
	const args = parseArgs(process.argv.slice(2));
	if (!args.version || !args["agent-digest"] || !args["memory-digest"]) {
		console.error(
			"usage: emit-manifest.mjs --version X.Y.Z --agent-digest sha256:... --memory-digest sha256:... " +
				"(--runtime-archive <path> | --runtime-digest sha256:...) [--kit-revision <sha>] [--write <path>]",
		);
		process.exit(1);
	}

	let runtimeDigest = args["runtime-digest"];
	if (!runtimeDigest) {
		const archivePath = args["runtime-archive"];
		if (!archivePath || !existsSync(archivePath)) {
			console.error(`runtime archive not found: ${archivePath ?? "(none given)"} — build it first (make runtime-archive)`);
			process.exit(1);
		}
		runtimeDigest = sha256File(archivePath);
	}

	const kitRevision = args["kit-revision"] || defaultKitRevision(process.cwd());

	const manifest = buildManifest({
		version: args.version,
		agentDigest: args["agent-digest"],
		memoryDigest: args["memory-digest"],
		runtimeDigest,
		kitRevision,
	});

	const json = `${JSON.stringify(manifest, null, 2)}\n`;
	if (args.write) {
		writeFileSync(args.write, json);
		console.log(`wrote ${args.write}`);
	} else {
		process.stdout.write(json);
	}
}

if (import.meta.url === `file://${process.argv[1]}`) {
	main();
}
