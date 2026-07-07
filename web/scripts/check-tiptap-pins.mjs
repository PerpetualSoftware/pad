#!/usr/bin/env node
// Drift guard for the coordinated Tiptap packages.
//
// The Y.Doc / ProseMirror schema is shared across these packages (see
// CLAUDE.md "Tiptap multi-package coordinated bumps"). Mixing versions can
// change the persisted Y.Doc shape silently, producing divergent ops the
// collab relay can't reconcile. To make that impossible via a stray
// `npm update`, every coordinated package MUST be exact-pinned in
// package.json and resolve to exactly one version in the lockfile.
//
// This script fails CI if either invariant is violated.

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const webDir = join(here, "..");

// The coordinated set. @tiptap/core, @tiptap/extension-collaboration and
// @tiptap/y-tiptap are the trio named in CLAUDE.md; @tiptap/pm carries the
// shared ProseMirror bundle they all build on, so it's pinned in lockstep.
const COORDINATED = [
	"@tiptap/core",
	"@tiptap/extension-collaboration",
	"@tiptap/y-tiptap",
	"@tiptap/pm",
];

// Exact semver: MAJOR.MINOR.PATCH with an optional prerelease/build suffix.
// Anything with a range operator (^ ~ >= etc.), an "x"/"*" wildcard, or a
// URL/tag spec is a non-exact pin and fails.
const EXACT = /^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z-.]+)?$/;

const pkg = JSON.parse(readFileSync(join(webDir, "package.json"), "utf8"));
const lock = JSON.parse(readFileSync(join(webDir, "package-lock.json"), "utf8"));

const errors = [];

for (const name of COORDINATED) {
	const declared = (pkg.dependencies && pkg.dependencies[name]) ??
		(pkg.devDependencies && pkg.devDependencies[name]);

	if (declared === undefined) {
		errors.push(`${name}: not found in package.json dependencies`);
		continue;
	}

	if (!EXACT.test(declared)) {
		errors.push(
			`${name}: declared as "${declared}" — must be an exact pin ` +
				`(e.g. "3.22.5"), no range operators`,
		);
	}

	// Collect every version this package resolves to across the whole
	// lockfile tree (top-level + any nested duplicate installs).
	const versions = new Set();
	for (const [path, meta] of Object.entries(lock.packages ?? {})) {
		if (
			(path === `node_modules/${name}` ||
				path.endsWith(`/node_modules/${name}`)) &&
			meta && meta.version
		) {
			versions.add(meta.version);
		}
	}

	if (versions.size === 0) {
		errors.push(`${name}: no resolved version found in package-lock.json`);
	} else if (versions.size > 1) {
		errors.push(
			`${name}: resolves to multiple versions in the lockfile ` +
				`[${[...versions].sort().join(", ")}] — coordinated packages ` +
				`must resolve to exactly one version`,
		);
	} else if (EXACT.test(declared) && ![...versions][0].startsWith(declared)) {
		// Exact pin present but the lockfile drifted away from it.
		errors.push(
			`${name}: package.json pins "${declared}" but the lockfile ` +
				`resolves "${[...versions][0]}"`,
		);
	}
}

if (errors.length > 0) {
	console.error("Tiptap coordinated-package pin check FAILED:\n");
	for (const e of errors) console.error(`  - ${e}`);
	console.error(
		"\nSee CLAUDE.md 'Tiptap multi-package coordinated bumps'. Bump all " +
			"coordinated packages together, exact-pinned to the same version, " +
			"then run `npm install` to refresh the lockfile.",
	);
	process.exit(1);
}

console.log(
	`Tiptap coordinated-package pin check OK (${COORDINATED.length} packages ` +
		`exact-pinned and single-versioned).`,
);
