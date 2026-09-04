#!/usr/bin/env node
// CI gate for `npm audit`, written so that a transport failure cannot be read
// as an advisory failure — and cannot be read as a pass either (docapp
// BUG-2881; codex round 1 on PR #1247).
//
// `npm audit` exits non-zero identically for "a HIGH/CRITICAL advisory exists
// in production deps" and "I could not reach the advisory service." On
// 2026-09-04 the registry's advisory endpoint had a bad window (one timeout,
// one 503, forty-three minutes apart) and both `main` and an open PR came back
// "Web: failure" — with Build, svelte-check and vitest all SKIPPED behind the
// `bash -e` step, so the lane carried zero frontend verification while reading
// like a check that ran and failed.
//
// This script runs the audit in --json mode and decides from the REPORT, not
// the exit code:
//
//   - a report with metadata.vulnerabilities → fail iff high + critical > 0,
//     naming the advisories (`::error title=npm audit`);
//   - an `error` envelope, or no parseable report → the advisory service could
//     not be asked. Retry with backoff (a registry blip is usually seconds
//     long), and if it still cannot be asked, FAIL with a distinct title
//     (`::error title=npm audit did not run`). Fail CLOSED, not open: the first
//     draft of this script warned and exited 0 here, and codex's round-1 read
//     was right that a security gate which passes when it cannot run is not a
//     gate. The two failure titles are the whole point — a reader can tell
//     "there is an advisory" from "the service was down" without opening the
//     log, and re-runs the second one instead of waving it through.
//
// The step also runs LAST in the Web job (see .github/workflows/ci.yml), so
// that whatever the audit does, Build / Type check / unit tests have already
// produced their result. That ordering is the blast-radius half of the fix;
// this script is the defect half.
//
// Usage:
//   node scripts/ci-audit.mjs            # run `npm audit --json` and decide
//   node scripts/ci-audit.mjs --input f  # decide from a saved report (tests)
//
// Env: CI_AUDIT_ATTEMPTS (default 3), CI_AUDIT_BACKOFF_MS (default 20000).

import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";

const AUDIT_ARGS = ["audit", "--json", "--audit-level=high", "--omit=dev"];
// Operator-set, but a typo must not turn into a TypeError or an infinite
// Atomics.wait (codex round 2): anything that is not an integer in range falls
// back to the default, and says so.
function envInt(name, fallback, min, max) {
	const raw = process.env[name];
	if (raw === undefined || raw === "") return fallback;
	const n = Number(raw);
	if (!Number.isInteger(n) || n < min || n > max) {
		console.log(
			`npm audit: ignoring ${name}=${JSON.stringify(raw)} (want an integer in [${min}, ${max}]); using ${fallback}`,
		);
		return fallback;
	}
	return n;
}
const ATTEMPTS = envInt("CI_AUDIT_ATTEMPTS", 3, 1, 10);
const BACKOFF_MS = envInt("CI_AUDIT_BACKOFF_MS", 20000, 0, 300000);

// A count the gate can decide from: a non-negative integer. Anything else — a
// string, null, NaN, a negative — is a report the gate cannot read, and an
// unreadable report takes the fail-closed path, never the clean one (codex
// round 2: `Number("x") + Number(null) > 0` is false, which read as "no
// advisories").
function isCount(v) {
	return Number.isInteger(v) && v >= 0;
}

function sleep(ms) {
	if (ms > 0)
		Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

// One attempt: the raw stdout and where it came from.
function fetchReport() {
	const at = process.argv.indexOf("--input");
	if (at !== -1) {
		return {
			raw: readFileSync(process.argv[at + 1], "utf8"),
			source: process.argv[at + 1],
		};
	}
	const res = spawnSync("npm", AUDIT_ARGS, {
		encoding: "utf8",
		shell: process.platform === "win32",
	});
	// stderr carries npm's own "npm error ..." lines on a transport failure;
	// surface it so the annotation is not the only trace.
	if (res.stderr && res.stderr.trim()) process.stderr.write(res.stderr);
	return { raw: res.stdout ?? "", source: `npm ${AUDIT_ARGS.join(" ")}` };
}

// Parse one attempt. Returns { vulns, report } when the service answered, or
// { unavailable: reason } when it did not — a retryable condition, not a
// verdict.
function readReport({ raw, source }) {
	let report;
	try {
		report = JSON.parse(raw);
	} catch {
		return {
			unavailable: `unparseable output from ${source} (${raw.trim().slice(0, 200) || "empty"})`,
		};
	}
	const vulns = report?.metadata?.vulnerabilities;
	if (!vulns) {
		// npm's --json error envelope: { message, error: { code, summary, detail } }.
		const code = report?.error?.code ? `${report.error.code}: ` : "";
		const why =
			report?.error?.summary ||
			report?.message ||
			"no metadata.vulnerabilities in the report";
		return { unavailable: `${code}${why}` };
	}
	if (!isCount(vulns.high) || !isCount(vulns.critical)) {
		return {
			unavailable: `metadata.vulnerabilities.high/critical are not counts (high=${JSON.stringify(vulns.high)}, critical=${JSON.stringify(vulns.critical)})`,
		};
	}
	return { vulns, report };
}

// --input is single-shot: a saved report is the same file every time.
const attempts = process.argv.includes("--input") ? 1 : ATTEMPTS;
let outcome;
for (let i = 1; i <= attempts; i++) {
	outcome = readReport(fetchReport());
	if (!outcome.unavailable) break;
	console.log(
		`npm audit: attempt ${i}/${attempts} — advisory service unavailable: ${outcome.unavailable}`,
	);
	if (i < attempts) sleep(BACKOFF_MS);
}

if (outcome.unavailable) {
	// FAIL CLOSED. The gate was not asked, so the job does not get to say it
	// passed. The title is distinct from the advisory failure below so the
	// checks tab tells the two apart; re-running the job is the remedy for
	// this one and never for that one.
	console.log(
		`::error title=npm audit did not run::${outcome.unavailable} (${attempts} attempt${attempts === 1 ? "" : "s"})`,
	);
	console.log(
		"npm audit: the advisory service could not be asked, so this is NOT a pass. Re-run the Web job.",
	);
	console.log(
		"npm audit: Build, Type check and unit tests above already ran; only the audit is outstanding.",
	);
	process.exit(1);
}

const { vulns, report } = outcome;
const high = vulns.high;
const critical = vulns.critical;
if (high + critical > 0) {
	const offenders = Object.values(report.vulnerabilities ?? {})
		.filter((v) => v.severity === "high" || v.severity === "critical")
		.map((v) => `${v.name} (${v.severity}${v.isDirect ? ", direct" : ""})`);
	console.log(
		`::error title=npm audit::${high} high, ${critical} critical advisor${high + critical === 1 ? "y" : "ies"} in production dependencies`,
	);
	for (const o of offenders) console.log(`  - ${o}`);
	console.log("Run `npm audit --omit=dev` locally for the full report.");
	process.exit(1);
}

console.log(
	`npm audit: 0 high, 0 critical in production dependencies (${vulns.total ?? 0} total across all levels).`,
);
