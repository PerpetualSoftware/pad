// TASK-4515: how the thread's rows group, where its day separators fall, and how the inbox
// writes a last-message time. Pure: no DOM, no state. Rows keep their own keys and ids; this
// only says which row shows chrome (`head`) and which row opens a local day (`day`).
import type { WindowItem } from './model';

/** Consecutive rows within this gap, same sender, same day and same source, are one group. */
export const GROUP_WINDOW_MS = 5 * 60 * 1000;

export interface RowLayout {
	/** The day separator's label before this row, or null for none. */
	day: string | null;
	/** True for the first row of a group: it shows the avatar, author, kind, time and source. */
	head: boolean;
}

function startOfDay(d: Date): Date {
	return new Date(d.getFullYear(), d.getMonth(), d.getDate());
}

/** Whole local calendar days from `d` to `now` (0 = same day, 1 = yesterday). DST-safe. */
function daysAgo(d: Date, now: Date): number {
	const a = startOfDay(d);
	const b = startOfDay(now);
	return Math.round((b.getTime() - a.getTime()) / 86_400_000);
}

function parse(iso: string): Date | null {
	if (!iso) return null;
	const d = new Date(iso);
	return Number.isNaN(d.getTime()) ? null : d;
}

/** "Today", "Yesterday", else "Wed, Nov 11" (with the year when it is not this year). */
export function dayLabel(iso: string, now: Date = new Date()): string {
	const d = parse(iso);
	if (!d) return '';
	const ago = daysAgo(d, now);
	if (ago === 0) return 'Today';
	if (ago === 1) return 'Yesterday';
	const opts: Intl.DateTimeFormatOptions = { weekday: 'short', month: 'short', day: 'numeric' };
	if (d.getFullYear() !== now.getFullYear()) opts.year = 'numeric';
	return d.toLocaleDateString(undefined, opts);
}

/** The inbox's time column: "9:40 AM" today, "Yesterday", "Mon" within a week, else "Nov 3". */
export function listTime(iso: string, now: Date = new Date()): string {
	const d = parse(iso);
	if (!d) return '';
	const ago = daysAgo(d, now);
	if (ago === 0) return d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
	if (ago === 1) return 'Yesterday';
	if (ago > 1 && ago < 7) return d.toLocaleDateString(undefined, { weekday: 'short' });
	const opts: Intl.DateTimeFormatOptions = { month: 'short', day: 'numeric' };
	if (d.getFullYear() !== now.getFullYear()) opts.year = 'numeric';
	return d.toLocaleDateString(undefined, opts);
}

interface Facts {
	sender: string;
	at: Date | null;
	source: string;
}

function facts(item: WindowItem, now: Date): Facts {
	if (item.kind === 'pending') return { sender: 'mine', at: now, source: '' };
	const m = item.message;
	const sender = m.mine ? 'mine' : `${m.author_is_human ? 'h' : 'a'}:${m.author_agent_key}`;
	return { sender, at: parse(m.created_at), source: m.context_workspace?.slug ?? '' };
}

/**
 * One layout per item. A row heads a group when it is the first row, follows the unread
 * divider, has a different sender or source than the row before, falls on another local day,
 * or is more than five minutes from the row before (either way: seq order can disagree with
 * created_at). A pending row is "now", from you, with no source of its own: it continues an own
 * group whose last row is pending or recent, and otherwise heads its own.
 */
export function layoutRows(
	items: readonly WindowItem[],
	opts: { dividerIndex?: number; now?: Date } = {}
): RowLayout[] {
	const now = opts.now ?? new Date();
	const divider = opts.dividerIndex ?? -1;
	const out: RowLayout[] = [];
	let prev: Facts | null = null;
	let prevItem: WindowItem | null = null;
	// The last row with a readable time: an unreadable one neither opens nor repeats a day.
	let lastDay: Date | null = null;
	for (let i = 0; i < items.length; i++) {
		const item = items[i];
		const f = facts(item, now);
		const newDay = !!f.at && (!lastDay || daysAgo(f.at, lastDay) !== 0);
		let head = true;
		if (prev?.at && prevItem && f.at && i !== divider && !newDay && f.sender === prev.sender) {
			const gap = Math.abs(f.at.getTime() - prev.at.getTime());
			const sameSource = item.kind === 'pending' || f.source === prev.source;
			head = !(gap <= GROUP_WINDOW_MS && sameSource);
			if (item.kind === 'pending' && prevItem.kind === 'pending') head = false;
		}
		out.push({ day: newDay ? dayLabel(f.at!.toISOString(), now) : null, head });
		if (f.at) lastDay = f.at;
		prev = f;
		prevItem = item;
	}
	return out;
}
