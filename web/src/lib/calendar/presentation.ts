import type { CalendarRow } from './monthGrid';
import type { AgendaEntry, AgendaWorkEntry, CommitmentForecastSummary, CommitmentRoles } from '$lib/types';

const DAY = 86_400_000;

function wallFormatter(zone: string) {
	return new Intl.DateTimeFormat('en-CA', { timeZone: zone, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' });
}

function wallText(format: Intl.DateTimeFormat, instant: number): string {
	const parts = Object.fromEntries(format.formatToParts(instant).map(p => [p.type, p.value]));
	return `${parts.year}-${parts.month}-${parts.day}T${parts.hour}:${parts.minute}:${parts.second}`;
}

export function todayIn(zone: string, now = new Date(Date.now())): string {
	return wallText(wallFormatter(zone), now.getTime()).slice(0, 10);
}

export function shiftDay(dateISO: string, days: number): string {
	const date = new Date(`${dateISO}T00:00:00Z`);
	date.setUTCDate(date.getUTCDate() + days);
	return date.toISOString().slice(0, 10);
}

/** Resolve a wall using nearby zone offsets: earlier on a fold, forward on a gap. */
export function wallToInstant(wall: string, zone: string): string {
	if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?$/.test(wall)) throw new RangeError('Invalid wall time');
	const normalized = wall.length === 16 ? `${wall}:00` : wall;
	const target = Date.parse(`${normalized}Z`);
	if (!Number.isFinite(target) || new Date(target).toISOString().slice(0, 19) !== normalized) throw new RangeError('Invalid wall time');
	const format = wallFormatter(zone);
	const offsets = new Set<number>();
	for (let hours = -48; hours <= 48; hours += 12) {
		const sample = target + hours * 3_600_000;
		offsets.add(Date.parse(`${wallText(format, sample)}Z`) - sample);
	}
	const candidates = [...offsets].map(offset => target - offset).sort((a, b) => a - b);
	const exact = candidates.find(candidate => wallText(format, candidate) === normalized);
	if (exact !== undefined) return new Date(exact).toISOString();
	const forward = candidates.map(instant => ({ instant, wall: wallText(format, instant) }))
		.filter(candidate => candidate.wall > normalized).sort((a, b) => a.wall.localeCompare(b.wall) || a.instant - b.instant)[0];
	if (!forward) throw new RangeError('Invalid wall time');
	return new Date(forward.instant).toISOString();
}

function startOfDay(day: string, zone: string): string {
	const format = wallFormatter(zone);
	const candidate = Date.parse(wallToInstant(`${day}T00:00`, zone));
	if (wallText(format, candidate) === `${day}T00:00:00`) return new Date(candidate).toISOString();
	// A midnight gap starts the day at its first valid instant, even if the gap began yesterday.
	let lo = candidate - 2 * DAY;
	let hi = candidate;
	while (hi - lo > 1) {
		const mid = Math.floor((hi + lo) / 2);
		if (wallText(format, mid).slice(0, 10) < day) lo = mid;
		else hi = mid;
	}
	return new Date(hi).toISOString();
}

export function dayWindow(dateISO: string, zone: string): { from: string; to: string } {
	return { from: startOfDay(dateISO, zone), to: startOfDay(shiftDay(dateISO, 1), zone) };
}

export function localTime(at: string, zone: string): string {
	return new Intl.DateTimeFormat('en-GB', { timeZone: zone, hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }).format(new Date(at));
}

export function localDate(at: string, zone: string): string {
	return new Intl.DateTimeFormat('en-US', { timeZone: zone, year: 'numeric', month: 'long', day: 'numeric' }).format(new Date(at));
}

export function dayLabel(day: string): string {
	return localDate(`${day}T12:00:00Z`, 'UTC');
}

/** A day as a heading: "Thursday 12 November", or "Thu 12 Nov" with both parts short. It never carries the year. */
export function longDay(day: string, weekday: 'long' | 'short' = 'long', month: 'long' | 'short' = 'long'): string {
	const parts = new Intl.DateTimeFormat('en-GB', { timeZone: 'UTC', weekday, day: 'numeric', month }).formatToParts(new Date(`${day}T12:00:00Z`));
	const part = (type: string) => parts.find(p => p.type === type)?.value;
	return `${part('weekday')} ${part('day')} ${part('month')}`;
}

export function instantPreview(at: string, zone: string): string {
	const offset = new Intl.DateTimeFormat('en-US', { timeZone: zone, timeZoneName: 'longOffset' }).formatToParts(new Date(at)).find(p => p.type === 'timeZoneName')?.value;
	return `Will be ${localDate(at, zone)}, ${localTime(at, zone)} (${offset})`;
}

export function isAllDay(entry: AgendaEntry): boolean {
	return entry.type === 'schedule' ? entry.all_day === true : entry.type === 'date' && /^\d{4}-\d{2}-\d{2}$/.test(entry.date ?? '');
}

/** A created or completed event: a record of what happened, never a commitment row. */
export function isEvent(entry: AgendaEntry): boolean {
	return entry.type === 'created' || entry.type === 'completed';
}

export function filterEntries(entries: AgendaEntry[], showEvents: boolean): AgendaEntry[] {
	return showEvents ? entries : entries.filter(entry => !isEvent(entry));
}

export function groupEntries(entries: AgendaEntry[]): { day: string; entries: AgendaEntry[] }[] {
	const groups = new Map<string, AgendaEntry[]>();
	for (const entry of entries) {
		const group = groups.get(entry.day) ?? [];
		group.push(entry);
		groups.set(entry.day, group);
	}
	return [...groups].sort(([a], [b]) => a.localeCompare(b)).map(([day, rows]) => ({
		day, entries: [...rows.filter(isAllDay), ...rows.filter(row => !isAllDay(row))]
	}));
}

export function entryKey(entry: AgendaEntry): string {
	return `${entry.type}|${entry.workspace}|${entry.owner_username}|${entry.item_ref}|${entry.schedule_id ?? entry.field ?? ''}|${entry.at}`;
}

export function entryHref(entry: AgendaEntry): string | null {
	const parts = [entry.owner_username, entry.workspace, entry.collection, entry.item_ref];
	return parts.every(Boolean) ? `/${parts.map(part => encodeURIComponent(part!)).join('/')}` : null;
}

export function canResolve(entry: AgendaEntry): boolean {
	return entry.type === 'schedule' && entry.state === 'outstanding';
}

export function canMove(entry: AgendaEntry): boolean {
	return canResolve(entry) && !(entry.roles?.deadline != null && entry.roles_origin !== 'native');
}

export type OccurrenceState = NonNullable<AgendaEntry['state']>;

/** The shape StateMark draws for a state. Colour follows the shape, not the chip tone. */
export type StateMarkShape = 'ring' | 'ring-thin' | 'ring-dashed' | 'check' | 'dash' | 'arrow' | 'cross';

export interface OccurrenceStateRow {
	/** Text for chips and the accessible name of the mark. */
	label: string;
	/** Chip colour, as an app.css custom property name. */
	tone: `--${string}`;
	mark: StateMarkShape;
}

/** The one table of occurrence states. Every chip, mark and label reads it. */
export const OCCURRENCE_STATES = {
	outstanding: { label: 'Outstanding', tone: '--accent-amber', mark: 'ring' },
	scheduled: { label: 'Scheduled', tone: '--status-blue', mark: 'ring-thin' },
	done: { label: 'Done', tone: '--accent-green', mark: 'check' },
	skipped: { label: 'Skipped', tone: '--accent-gray', mark: 'dash' },
	moved: { label: 'Moved', tone: '--accent-purple', mark: 'arrow' },
	missed: { label: 'Missed', tone: '--accent-red', mark: 'cross' },
	lapsed: { label: 'Lapsed', tone: '--accent-red', mark: 'cross' },
	unrecorded: { label: 'Not recorded', tone: '--accent-gray', mark: 'ring-dashed' }
} as const satisfies Record<OccurrenceState, OccurrenceStateRow>;

/** The table row for a loosely typed state value, or undefined when it is not a known state. */
export function occurrenceState(state: string | null | undefined): OccurrenceStateRow | undefined {
	return state != null && Object.hasOwn(OCCURRENCE_STATES, state) ? OCCURRENCE_STATES[state as OccurrenceState] : undefined;
}

export function entryChips(entry: AgendaEntry): { text: string; color: string }[] {
	const chip = (text: string, color = '--accent-gray') => ({ text, color: `var(${color})` });
	if (entry.type === 'created') return [chip('Created')];
	if (entry.type === 'completed') return [chip('Completed')];
	if (entry.type === 'date') {
		const label = entry.field === 'due_date' ? 'Due' : entry.field === 'end_date' ? 'Ends' : entry.field ?? '';
		return [chip(label), ...(entry.overdue ? [chip('Overdue', '--accent-amber')] : [])];
	}
	const chips = entry.state ? [chip(OCCURRENCE_STATES[entry.state].label, OCCURRENCE_STATES[entry.state].tone)] : [];
	if (entry.state === 'outstanding') {
		if (entry.overdue) chips.push(chip('Overdue', '--accent-amber'));
		if (entry.missed_count && entry.missed_count > 0) chips.push(chip(`${entry.missed_count} missed occurrence${entry.missed_count === 1 ? '' : 's'}`, '--accent-amber'));
		if (entry.stale) chips.push(chip('Stale', '--accent-amber'));
	}
	return chips;
}

export type AgendaAction = 'done' | 'skip' | 'snooze' | 'move' | 'today' | 'forecast';

export function weekWindow(start: string, zone: string): { from: string; to: string } {
	return { from: dayWindow(start, zone).from, to: dayWindow(shiftDay(start, 7), zone).from };
}

/** The week from `start`. `short` abbreviates the months and leaves the year out, for narrow screens. */
export function weekLabel(start: string, short = false): string {
	const end = shiftDay(start, 6);
	const [year, month, date] = start.split('-');
	const [endYear, endMonth, endDate] = end.split('-');
	const monthName = (day: string) => new Intl.DateTimeFormat('en-GB', { month: short ? 'short' : 'long', timeZone: 'UTC' }).format(new Date(`${day}T12:00Z`));
	if (short) return month === endMonth ? `${+date}–${+endDate} ${monthName(end)}` : `${+date} ${monthName(start)} – ${+endDate} ${monthName(end)}`;
	if (year !== endYear) return `${+date} ${monthName(start)} ${year} – ${+endDate} ${monthName(end)} ${endYear}`;
	if (month !== endMonth) return `${+date} ${monthName(start)} – ${+endDate} ${monthName(end)} ${year}`;
	return `${+date}–${+endDate} ${monthName(end)} ${year}`;
}

export function dateTime(at: string, zone: string, allDay = false): string {
	return `${localDate(at, zone)}${allDay ? '' : `, ${localTime(at, zone)}`}`;
}

export function targetOnDay(entry: AgendaEntry, day: string, zone: string): string {
	return wallToInstant(`${day}T${isAllDay(entry) ? '00:00' : localTime(entry.at, zone)}`, zone);
}

/** A date-only role value (`YYYY-MM-DD`) is a calendar date, never a UTC instant. */
export function isDateOnly(value: string): boolean {
	return /^\d{4}-\d{2}-\d{2}$/.test(value);
}

/** The viewer-local day a role value falls on. */
export function roleDay(value: string, zone: string): string {
	return isDateOnly(value) ? value : todayIn(zone, new Date(value));
}

/** A role value as text: a date-only value shows its date alone. */
export function roleText(value: string, zone: string): string {
	return isDateOnly(value) ? dayLabel(value) : dateTime(value, zone);
}

/** Whether instant names the current target. A date-only target matches local midnight of its date in zone. */
export function sameTarget(current: string, instant: string, zone: string): boolean {
	const at = Date.parse(instant);
	if (!Number.isFinite(at)) return false;
	if (isDateOnly(current)) return at === Date.parse(wallToInstant(`${current}T00:00`, zone));
	return at === Date.parse(current);
}

export function forecastSetter(f: CommitmentForecastSummary): string {
 return f.set_by_name ? `${f.set_by_name}${f.set_by_kind === 'agent' ? ' (agent)' : ''}` : f.set_by_kind === 'agent' ? 'an agent' : 'a member';
}
export function forecastText(f: CommitmentForecastSummary, zone: string): string {
 return `Forecast ${roleText(f.value, zone)} · set by ${forecastSetter(f)}`;
}

/** Preserve authoritative placement and server order within all-day/timed groups. */
export function agendaRows(entries: AgendaEntry[]): CalendarRow[] {
 return groupEntries(entries).flatMap(group => group.entries.map(entry => ({
  key: entryKey(entry), day: entry.day, ref: entry.item_ref, target: entry.item_ref, title: entry.item_title,
  href: entryHref(entry), workspaceName: entry.workspace_name, allDay: isAllDay(entry), at: entry.at,
  ...(entry.type === 'schedule'
   ? { state: entry.state, overdue: entry.overdue, missedCount: entry.missed_count, stale: entry.stale, forecast: entry.forecast }
   : { label: entryChips(entry)[0]?.text, overdue: entry.overdue })
 })));
}

export type AttentionReason = 'deadline_still_owed' | 'target_passed' | 'lapsed' | 'missed';

export interface AttentionEntry {
	key: string;
	reason: AttentionReason;
	/** The window entry when the occurrence is in the window, else the priority-list row. */
	entry: AgendaEntry | AgendaWorkEntry;
	inWindow: boolean;
}

const ATTENTION_ORDER: AttentionReason[] = ['deadline_still_owed', 'target_passed', 'lapsed', 'missed'];

/** One schedule has at most one outstanding occurrence, so that occurrence is keyed by its schedule. */
function attentionKey(entry: AgendaEntry | AgendaWorkEntry): string {
	const occurrence = entry.state === 'outstanding' ? 'outstanding' : entry.occurrence_key ?? entry.at ?? '';
	return `${entry.workspace ?? ''}|${entry.owner_username ?? ''}|${entry.item_ref}|${entry.schedule_id ?? ''}|${occurrence}`;
}

/**
 * What needs attention: priority-list rows whose target has passed or whose deadline passed
 * while still owed, plus window occurrences that lapsed, were missed or carry missed occurrences.
 * Each occurrence is listed once, under its most severe reason.
 */
export function attentionEntries(entries: AgendaEntry[], priorityList: AgendaWorkEntry[]): AttentionEntry[] {
	const found = new Map<string, AttentionEntry>();
	const add = (reason: AttentionReason, entry: AgendaEntry | AgendaWorkEntry, inWindow: boolean) => {
		const key = attentionKey(entry);
		const seen = found.get(key);
		if (!seen) { found.set(key, { key, reason, entry, inWindow }); return; }
		if (ATTENTION_ORDER.indexOf(reason) < ATTENTION_ORDER.indexOf(seen.reason)) seen.reason = reason;
		if (inWindow && !seen.inWindow) { seen.entry = entry; seen.inWindow = true; }
	};
	for (const row of priorityList) {
		if (row.type !== 'schedule') continue;
		if (row.reason === 'deadline_still_owed' || row.reason === 'target_passed') add(row.reason, row, false);
	}
	for (const entry of entries) {
		if (entry.type !== 'schedule') continue;
		if (entry.state === 'lapsed') add('lapsed', entry, true);
		else if (entry.state === 'missed') add('missed', entry, true);
		else if (entry.missed_count && entry.missed_count > 0) add('missed', entry, true);
		else if (found.has(attentionKey(entry))) add(found.get(attentionKey(entry))!.reason, entry, true);
	}
	return [...found.values()].sort((a, b) => ATTENTION_ORDER.indexOf(a.reason) - ATTENTION_ORDER.indexOf(b.reason));
}

/** One line for a day heading: "2 owed · 1 done". Empty when the day has nothing owed, done or missed. */
export function daySummary(entries: AgendaEntry[]): string {
	const count = (...states: OccurrenceState[]) => entries.filter(entry => entry.type === 'schedule' && entry.state !== undefined && states.includes(entry.state)).length;
	return ([[count('outstanding'), 'owed'], [count('done'), 'done'], [count('lapsed', 'missed'), 'missed']] as const)
		.filter(([n]) => n > 0).map(([n, word]) => `${n} ${word}`).join(' · ');
}

function dayNumber(day: string): number {
	return Date.parse(`${day}T00:00:00Z`) / DAY;
}

/**
 * A calendar day in the fewest words that stay unambiguous from `today`:
 * "Mon 16" within a week either side, "Fri 20 Nov" in the same year, else "Wed 20 Jan 2027".
 */
export function shortDay(day: string, today: string): string {
	const near = Math.abs(dayNumber(day) - dayNumber(today)) <= 7;
	const parts = new Intl.DateTimeFormat('en-GB', {
		timeZone: 'UTC', weekday: 'short', day: 'numeric',
		...(near ? {} : { month: 'short' }), ...(near || day.slice(0, 4) === today.slice(0, 4) ? {} : { year: 'numeric' })
	}).formatToParts(new Date(`${day}T12:00:00Z`));
	const part = (type: string) => parts.find(p => p.type === type)?.value;
	return [part('weekday'), part('day'), part('month'), part('year')].filter(Boolean).join(' ');
}

/** A role value in short form: a date-only value is its own date, an instant is read in the viewer zone. */
export function shortRoleDate(value: string, zone: string, now = new Date(Date.now())): string {
	const day = shortDay(roleDay(value, zone), todayIn(zone, now));
	return isDateOnly(value) ? day : `${day}, ${localTime(value, zone)}`;
}

export type DeadlineTone = 'far' | 'soon' | 'passed';

const SOON = 2 * DAY;

/** The instant a deadline passes. A date-only deadline holds through its whole local day. */
function deadlineInstant(deadline: string, zone: string): number {
	return Date.parse(isDateOnly(deadline) ? startOfDay(shiftDay(deadline, 1), zone) : deadline);
}

export function deadlineTone(deadline: string, zone: string, now = new Date(Date.now())): DeadlineTone {
	const left = deadlineInstant(deadline, zone) - now.getTime();
	return left <= 0 ? 'passed' : left <= SOON ? 'soon' : 'far';
}

function elapsedText(ms: number): string {
	if (ms < 3_600_000) return `${Math.max(1, Math.floor(ms / 60_000))} min`;
	if (ms < DAY) return `${Math.floor(ms / 3_600_000)} h`;
	const days = Math.floor(ms / DAY);
	return `${days} day${days === 1 ? '' : 's'}`;
}

/** Tone and wording of the deadline pill. The behaviour is appended only once it matters. */
export function deadlinePill(deadline: string, behavior: CommitmentRoles['deadline_behavior'], zone: string, now = new Date(Date.now())): { tone: DeadlineTone; text: string } {
	const tone = deadlineTone(deadline, zone, now);
	if (tone === 'passed') {
		return { tone, text: `Deadline passed ${elapsedText(now.getTime() - deadlineInstant(deadline, zone))} ago${behavior === 'stays_owed' ? ' · still owed' : ''}` };
	}
	const suffix = tone === 'soon' ? (behavior === 'lapses' ? ' · lapses' : behavior === 'stays_owed' ? ' · still owed after' : '') : '';
	return { tone, text: `${shortRoleDate(deadline, zone, now)}${suffix}` };
}

/**
 * How many viewer-local days a forecast lands after its deadline, or null when it is not late.
 * Two instants compare exactly, so 0 means later on the deadline day. A date-only side compares whole days.
 */
export function forecastLate(forecast: string, deadline: string | null | undefined, zone: string): number | null {
	if (!deadline) return null;
	const days = dayNumber(roleDay(forecast, zone)) - dayNumber(roleDay(deadline, zone));
	if (isDateOnly(forecast) || isDateOnly(deadline)) return days > 0 ? days : null;
	return Date.parse(forecast) > Date.parse(deadline) ? Math.max(0, days) : null;
}

/** Wording of the forecast pill, and whether it lands after the deadline. */
export function forecastPill(forecast: string, deadline: string | null | undefined, zone: string, now = new Date(Date.now())): { late: boolean; text: string } {
	const days = forecastLate(forecast, deadline, zone);
	const text = shortRoleDate(forecast, zone, now);
	if (days === null) return { late: false, text };
	return { late: true, text: `${text} · ${days === 0 ? 'late' : `${days} day${days === 1 ? '' : 's'} late`}` };
}

/** A rail row as the outstanding occurrence it stands for, or null when it has no target to act on. */
export function workOccurrence(row: AgendaWorkEntry, zone: string): AgendaEntry | null {
	if (row.type !== 'schedule' || !row.roles?.target) return null;
	const target = row.roles.target;
	const at = isDateOnly(target) ? wallToInstant(`${target}T00:00`, zone) : target;
	return { ...row, type: 'schedule', state: 'outstanding', at, day: roleDay(target, zone), all_day: row.all_day ?? isDateOnly(target) };
}

export interface RoleMarkers { deadlines: AgendaEntry[]; forecasts: AgendaEntry[] }

/**
 * Deadlines and forecasts that land on a different day from their target, keyed by the day they land on.
 * Deadlines come from live occurrences only; a schedule's forecast is marked once.
 */
export function roleMarkers(entries: AgendaEntry[], zone: string): Map<string, RoleMarkers> {
	const markers = new Map<string, RoleMarkers>();
	const on = (day: string) => markers.get(day) ?? markers.set(day, { deadlines: [], forecasts: [] }).get(day)!;
	const forecast = new Set<string>();
	for (const entry of entries) {
		const deadline = entry.type === 'schedule' && (entry.state === 'outstanding' || entry.state === 'scheduled') ? entry.roles?.deadline : null;
		if (deadline && roleDay(deadline, zone) !== entry.day) on(roleDay(deadline, zone)).deadlines.push(entry);
		if (!entry.forecast || roleDay(entry.forecast.value, zone) === entry.day) continue;
		const schedule = JSON.stringify([entry.workspace, entry.owner_username, entry.schedule_id]);
		if (forecast.has(schedule)) continue;
		forecast.add(schedule); on(roleDay(entry.forecast.value, zone)).forecasts.push(entry);
	}
	return markers;
}
