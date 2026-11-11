import type { AgendaEntry } from '$lib/types';

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

export function instantPreview(at: string, zone: string): string {
	const offset = new Intl.DateTimeFormat('en-US', { timeZone: zone, timeZoneName: 'longOffset' }).formatToParts(new Date(at)).find(p => p.type === 'timeZoneName')?.value;
	return `Will be ${localDate(at, zone)}, ${localTime(at, zone)} (${offset})`;
}

export function isAllDay(entry: AgendaEntry): boolean {
	return entry.type === 'schedule' ? entry.all_day === true : entry.type === 'date' && /^\d{4}-\d{2}-\d{2}$/.test(entry.date ?? '');
}

export function filterEntries(entries: AgendaEntry[], showEvents: boolean): AgendaEntry[] {
	return showEvents ? entries : entries.filter(entry => entry.type !== 'created' && entry.type !== 'completed');
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

export function entryChips(entry: AgendaEntry): { text: string; color: string }[] {
	const chip = (text: string, color = '--accent-gray') => ({ text, color: `var(${color})` });
	if (entry.type === 'created') return [chip('Created')];
	if (entry.type === 'completed') return [chip('Completed')];
	if (entry.type === 'date') {
		const label = entry.field === 'due_date' ? 'Due' : entry.field === 'end_date' ? 'Ends' : entry.field ?? '';
		return [chip(label), ...(entry.overdue ? [chip('Overdue', '--accent-amber')] : [])];
	}
	const states = {
		outstanding: chip('Outstanding', '--accent-amber'), scheduled: chip('Scheduled', '--status-blue'),
		done: chip('Done', '--accent-green'), skipped: chip('Skipped'), moved: chip('Moved', '--accent-purple'),
		missed: chip('Missed', '--accent-amber'), lapsed: chip('Lapsed'), unrecorded: chip('No recorded state')
	};
	const chips = entry.state ? [states[entry.state]] : [];
	if (entry.state === 'outstanding') {
		if (entry.overdue) chips.push(chip('Overdue', '--accent-amber'));
		if (entry.missed_count && entry.missed_count > 0) chips.push(chip(`${entry.missed_count} missed occurrence${entry.missed_count === 1 ? '' : 's'}`, '--accent-amber'));
		if (entry.stale) chips.push(chip('Stale', '--accent-amber'));
	}
	return chips;
}
