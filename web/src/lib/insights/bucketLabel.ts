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
//  - A DAY bucket is labelled by its date ("7/19"). It is still the UTC
//    calendar day: making a day a local day needs the server to bucket in the
//    viewer's zone, which is a separate change (filed from TASK-2220).
//  - Anything else (a future week or month key) is shown as it came.
//
// Labels are the chart's band-scale domain, and a band scale merges equal
// values into one band. A DST fall-back repeats a local hour, so a repeated
// label gets a numbered suffix rather than silently sharing a bar.

const HOUR = /^(\d{4})-(\d{2})-(\d{2})T(\d{2})$/;
const DAY = /^(\d{4})-(\d{2})-(\d{2})$/;

export function bucketLabel(key: string): string {
	const h = HOUR.exec(key);
	if (h) {
		const at = new Date(Date.UTC(+h[1], +h[2] - 1, +h[3], +h[4]));
		return `${at.getMonth() + 1}/${at.getDate()} ${at.getHours()}h`;
	}
	const d = DAY.exec(key);
	if (d) return `${+d[2]}/${+d[3]}`;
	return key;
}

/** bucketLabel over a series, with any repeated label made unique. */
export function bucketLabels(keys: string[]): string[] {
	const seen = new Map<string, number>();
	return keys.map((key) => {
		const label = bucketLabel(key);
		const n = (seen.get(label) ?? 0) + 1;
		seen.set(label, n);
		return n === 1 ? label : `${label} (${n})`;
	});
}
