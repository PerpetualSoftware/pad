// The throughput chart's bucket labels (TASK-2220, audit C49).
//
// The server buckets by UTC and names each bucket by a sortable key:
// `2026-07-19T16` for an hour (the Day window), `2026-07-19` for a day. The
// Insights page drew those keys verbatim, and an hour key is a UTC hour, so a
// reader west of Greenwich saw this afternoon's work under tomorrow's date.
// The print report already shortened the keys; both views now share this.
//
//  - An HOUR bucket is labelled by its start in the viewer's local time
//    ("7/19 9h" for 16:00 UTC in UTC-7). An hour is an hour in any zone with a
//    whole-hour offset, so the count under the label is exactly the work
//    done in that local hour.
//  - A DAY bucket is labelled by its date ("7/19"). Since TASK-3524 the page
//    sends the viewer's zone and the server buckets local days; against an
//    older server it is the UTC calendar day.
//  - Anything else (a future week or month key) is shown as it came.
//
// Labels are the chart's band-scale domain, and a band scale merges equal
// values into one band. A DST fall-back repeats a local hour, so a repeated
// label gets a numbered suffix rather than silently sharing a bar.

const HOUR = /^(\d{4})-(\d{2})-(\d{2})T(\d{2})$/;
const DAY = /^(\d{4})-(\d{2})-(\d{2})$/;

/**
 * The browser's IANA zone, sent as the report's `tz` so the server buckets in
 * it (TASK-3524). Undefined where Intl cannot say, which leaves UTC buckets.
 */
export function viewerTimeZone(): string | undefined {
	try {
		return Intl.DateTimeFormat().resolvedOptions().timeZone || undefined;
	} catch {
		return undefined;
	}
}

/**
 * `keysAreLocal`: the server bucketed in the viewer's zone (the report echoes
 * `tz`, TASK-3524), so an hour key is already a local hour and is only
 * reformatted. A server without that echo sends UTC keys, which are converted
 * here as before (version skew: a newer page against an older server).
 */
export function bucketLabel(key: string, keysAreLocal = false): string {
	const h = HOUR.exec(key);
	if (h) {
		if (keysAreLocal) return `${+h[2]}/${+h[3]} ${+h[4]}h`;
		const at = new Date(Date.UTC(+h[1], +h[2] - 1, +h[3], +h[4]));
		return `${at.getMonth() + 1}/${at.getDate()} ${at.getHours()}h`;
	}
	const d = DAY.exec(key);
	if (d) return `${+d[2]}/${+d[3]}`;
	return key;
}

/** bucketLabel over a series, with any repeated label made unique. */
export function bucketLabels(keys: string[], keysAreLocal = false): string[] {
	const seen = new Map<string, number>();
	return keys.map((key) => {
		const label = bucketLabel(key, keysAreLocal);
		const n = (seen.get(label) ?? 0) + 1;
		seen.set(label, n);
		return n === 1 ? label : `${label} (${n})`;
	});
}
