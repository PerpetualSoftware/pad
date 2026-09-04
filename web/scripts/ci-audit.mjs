#!/usr/bin/env node
// CI gate for `npm audit`, written so that a transport failure cannot be read
// as an advisory failure (docapp BUG-2881).
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
//     naming the advisories (the gate that was always intended);
//   - a report with an `error` envelope, or no parseable report at all → the
//     advisory service could not be asked. Emit a GitHub warning annotation
//     that says exactly that and exit 0, so the lane's other steps still stand
//     as the frontend's verdict and the missing audit is visible as MISSING
//     rather than disguised as a failure.
//
// The step also runs LAST in the Web job (see .github/workflows/ci.yml), so
// that whatever the audit does, Build / Type check / unit tests have already
// produced their result. That ordering is the blast-radius half of the fix;
// this script is the defect half.
//
// Usage:
//   node scripts/ci-audit.mjs            # run `npm audit --json` and decide
//   node scripts/ci-audit.mjs --input f  # decide from a saved report (tests)

import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";

const AUDIT_ARGS = ["audit", "--json", "--audit-level=high", "--omit=dev"];

function loadReport() {
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

function warnUnavailable(reason) {
	// GitHub Actions workflow command: shows on the job summary and the
	// checks tab without turning the step red.
	console.log(`::warning title=npm audit did not run::${reason}`);
	console.log(`npm audit: advisory service unavailable — ${reason}`);
	console.log(
		"npm audit: NOT a pass. The gate was not asked; re-run the Web job to ask it.",
	);
}

const { raw, source } = loadReport();

let report;
try {
	report = JSON.parse(raw);
} catch {
	warnUnavailable(
		`unparseable output from ${source} (${raw.trim().slice(0, 200) || "empty"})`,
	);
	process.exit(0);
}

const vulns = report?.metadata?.vulnerabilities;
if (!vulns) {
	// npm's --json error envelope: { message, error: { code, summary, detail } }.
	const code = report?.error?.code ? `${report.error.code}: ` : "";
	const why =
		report?.error?.summary ||
		report?.message ||
		"no metadata.vulnerabilities in the report";
	warnUnavailable(`${code}${why}`);
	process.exit(0);
}

const high = Number(vulns.high ?? 0);
const critical = Number(vulns.critical ?? 0);
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
