// Defer presets for the needs-you inbox (TASK-3923). Everything is computed in
// the viewer's LOCAL time and sent as an RFC3339 instant: "tomorrow 09:00"
// means 09:00 on this person's wall clock, not UTC.

const HOUR = 9;

/** The local date at 09:00, `days` after `now`'s date. */
function atNine(now: Date, days: number): Date {
	return new Date(now.getFullYear(), now.getMonth(), now.getDate() + days, HOUR, 0, 0, 0);
}

export function tomorrowAt9(now: Date = new Date()): Date {
	return atNine(now, 1);
}

/** The next Monday strictly after today (a Monday asks for the one a week out), 09:00. */
export function nextMondayAt9(now: Date = new Date()): Date {
	const day = now.getDay(); // 0 Sunday .. 6 Saturday
	const ahead = ((1 - day + 7) % 7) || 7;
	return atNine(now, ahead);
}

/**
 * `YYYY-MM-DD` (an <input type=date> value) at 09:00 local, or null when blank
 * or not a real calendar date.
 */
export function dateAt9(value: string, _now: Date = new Date()): Date | null {
	const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value);
	if (!m) return null;
	const y = Number(m[1]);
	const mo = Number(m[2]) - 1;
	const d = Number(m[3]);
	const out = new Date(y, mo, d, HOUR, 0, 0, 0);
	if (out.getFullYear() !== y || out.getMonth() !== mo || out.getDate() !== d) return null;
	return out;
}

/** `YYYY-MM-DD` of a local date, for an <input type=date> min. */
export function localDateValue(d: Date): string {
	const p = (n: number) => String(n).padStart(2, '0');
	return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}
