// Pure lane logic for the Home Agents panel (DOC-4851 section 4). A lane's
// display is its queue in order plus the park line as a position among the
// items; every move is planned here, with the line as an immovable card.

import { planLaneOrder, type LanePlan } from '$lib/collections/reorderPlan';
import type { LaneOverlay } from '$lib/stores/agentsOverview.svelte';
import type { RoleParkRequest, RoleQueueItem, RoleQueueResponse } from '$lib/types';

export const LINE = '\u0000line';

/** The queue as shown, and the line's position among those items (null: not shown). */
export interface LaneView {
	items: RoleQueueItem[];
	line: number | null;
	mode: 'top' | 'line' | null;
}

/** Where the server puts the line within the returned prefix. */
export function serverLine(lane: RoleQueueResponse): number | null {
	const park = lane.park;
	if (!park) return null;
	if (park.mode === 'top') return 0;
	if (park.index === null) return lane.queue_more ? null : lane.queue.length;
	return park.index <= lane.queue.length ? park.index : null;
}

export function laneView(lane: RoleQueueResponse, overlay?: LaneOverlay): LaneView {
	const mode = lane.park?.mode ?? null;
	if (!overlay) return { items: lane.queue, line: serverLine(lane), mode };
	const byRef = new Map(lane.queue.map(item => [item.ref, item]));
	const items = overlay.refs.flatMap(ref => byRef.get(ref) ?? []);
	return { items, line: overlay.line === null ? null : Math.min(overlay.line, items.length), mode };
}

/** The view as one token list: refs, with LINE where the line sits. */
export function tokens(view: LaneView): string[] {
	const out = view.items.map(item => item.ref);
	if (view.line !== null) out.splice(view.line, 0, LINE);
	return out;
}

export function fromTokens(list: readonly string[]): LaneOverlay {
	const at = list.indexOf(LINE);
	return { refs: list.filter(t => t !== LINE), line: at < 0 ? null : at };
}

/** Move the token at `from` so it lands at index `to` of the result. */
export function moveToken(list: readonly string[], from: number, to: number): string[] {
	const next = list.slice();
	const [token] = next.splice(from, 1);
	next.splice(Math.max(0, Math.min(to, next.length)), 0, token);
	return next;
}

/** Do next: the item goes to queue index 0, never above a top line. */
export function doNextOrder(view: LaneView, ref: string): LaneOverlay {
	const list = tokens(view);
	const from = list.indexOf(ref);
	const to = view.mode === 'top' ? 1 : 0;
	return fromTokens(moveToken(list, from, to));
}

/**
 * Move up or down one place. An item moves past the line like a drag across
 * it; a top line stays first, so nothing moves above it.
 */
export function stepOrder(view: LaneView, ref: string, delta: -1 | 1): LaneOverlay | null {
	const list = tokens(view);
	const from = list.indexOf(ref);
	const to = from + delta;
	const floor = view.mode === 'top' ? 1 : 0;
	if (from < 0 || to < floor || to >= list.length) return null;
	return fromTokens(moveToken(list, from, to));
}

/** The park request for the line placed at `line` among `refs`. */
export function lineRequest(refs: readonly string[], line: number): RoleParkRequest | null {
	if (!refs.length) return null;
	return line < refs.length ? { before: refs[line] } : { after: refs[refs.length - 1] };
}

/**
 * The role_sort_order writes for a lane in its new order. The line, in mode
 * line, is a view-only card just below its position P, so the items above it
 * stay below P and the items at or past it stay parked. In mode top the line
 * holds no position: every item is parked whatever its order.
 *
 * A truncated lane (queue_more) has items past the shown ones, so its end is
 * not open: `bound`, the first item not shown, closes it as a frozen card, and
 * a line beyond the shown items (next.line null) is placed beside it by value.
 * Without them a move to the last slot could tie or pass the hidden item, or
 * cross the line unseen.
 */
export function planLane(
	lane: RoleQueueResponse,
	next: LaneOverlay,
	movedRef: string,
	lineSort: number | null,
	bound: RoleQueueItem | null = null
): LanePlan {
	const byRef = new Map(lane.queue.map(item => [item.ref, item]));
	const cards: { id: string; sort_order: number }[] = next.refs.map(ref => ({ id: ref, sort_order: byRef.get(ref)?.role_sort_order ?? 0 }));
	const line = lane.park?.mode === 'line' && lineSort !== null ? { id: LINE, sort_order: lineSort - 1 } : null;
	const tail: { id: string; sort_order: number }[] = [];
	if (line && next.line !== null) cards.splice(next.line, 0, line);
	else if (line) tail.push(line);
	if (bound) tail.push({ id: LINE + bound.ref, sort_order: bound.role_sort_order });
	tail.sort((a, b) => a.sort_order - b.sort_order);
	cards.push(...tail);
	return planLaneOrder(cards, card => !card.id.startsWith(LINE) && lane.can_reorder && !!byRef.get(card.id)?.can_edit, movedRef);
}

export type BoundedLane = { ok: true; lane: RoleQueueResponse; bound: RoleQueueItem | null } | { ok: false };

/**
 * A truncated lane read again at write time with `shown + 1` items: the shown
 * items take the fresh values, and the next item becomes the bound. Not ok when
 * the fresh prefix no longer matches what the user saw (someone reordered).
 */
export function boundLane(lane: RoleQueueResponse, shownRefs: readonly string[], fresh: RoleQueueResponse): BoundedLane {
	const prefix = fresh.queue.slice(0, shownRefs.length);
	if (prefix.length !== shownRefs.length || prefix.some((item, i) => item.ref !== shownRefs[i])) return { ok: false };
	const byRef = new Map(prefix.map(item => [item.ref, item]));
	return {
		ok: true,
		lane: { ...lane, queue: lane.queue.map(item => byRef.get(item.ref) ?? item) },
		bound: fresh.queue[shownRefs.length] ?? null
	};
}

/** "parked", "parked k/n" across an agent's lanes, or '' when none is. */
export function parkedLabel(lanes: readonly RoleQueueResponse[]): string {
	const parked = lanes.filter(lane => lane.park).length;
	if (!parked) return '';
	return parked === lanes.length ? 'parked' : `parked ${parked}/${lanes.length}`;
}

/** A compact age: 25m, 2h, 3d. */
export function age(since: string, now = Date.now()): string {
	const minutes = Math.max(0, Math.floor((now - Date.parse(since)) / 60000));
	if (minutes < 60) return `${minutes}m`;
	const hours = Math.floor(minutes / 60);
	if (hours < 24) return `${hours}h`;
	return `${Math.floor(hours / 24)}d`;
}

/** "Stop here. Set by Dave 15:04", the time in the viewer's zone (with the date when not today). */
export function lineLabel(setBy: string | null, setAt: string | null, zone: string, now = new Date()): string {
	if (!setAt) return setBy ? `Stop here. Set by ${setBy}` : 'Stop here';
	const at = new Date(setAt);
	const day = (d: Date) => new Intl.DateTimeFormat('en-CA', { timeZone: zone, year: 'numeric', month: '2-digit', day: '2-digit' }).format(d);
	const time = new Intl.DateTimeFormat('en-GB', { timeZone: zone, hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }).format(at);
	const when = day(at) === day(now) ? time : `${new Intl.DateTimeFormat('en-US', { timeZone: zone, month: 'short', day: 'numeric' }).format(at)} ${time}`;
	return setBy ? `Stop here. Set by ${setBy} ${when}` : `Stop here. Set ${when}`;
}
