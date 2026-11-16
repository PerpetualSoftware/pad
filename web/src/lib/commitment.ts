import type { AcknowledgedBy, AgendaEntry, Collection, CommitmentCreateRequest, CommitmentGate, CommitmentGateView, CommitmentLink, CommitmentRenegotiateRequest, CommitmentRoles, CommitmentUpdateRequest, Item, ItemCommitmentView } from '$lib/types';
import { isDateOnly, roleDay, todayIn, wallToInstant, type OccurrenceState } from '$lib/calendar/presentation';

export function commitmentKind(roles: Pick<CommitmentRoles, 'deadline' | 'occupied_until'>): 'deadline_hard' | 'window' | 'check' {
 if (roles.deadline !== null) return 'deadline_hard';
 if (roles.occupied_until !== null) return 'window';
 return 'check';
}


/** undefined is loading/unavailable; null is an authoritative not_found. */
export interface ItemCommitmentSnapshot {
 wsSlug: string;
 itemId: string;
 itemSlug: string;
 authEpoch: number;
 zone: string;
 view: ItemCommitmentView | null | undefined;
 /** The path item's gates GET; set only when view is null (no commitment). */
 gates?: CommitmentGateView;
 /** The server's display hint: may this caller write the commitment. Absent on the wire means false. */
 editable: boolean;
}

/**
 * The commitment's current occurrence as an Agenda entry, so the Agenda's
 * occurrence actions and dialogs work unchanged. Only authoritative values:
 * `fired` is unknown here, so it is omitted and Acknowledge is not offered.
 * A date-only target is keyed at midnight in the schedule's zone (the server's
 * placement); `at` is only a key, since resolve re-reads next_occurrence.at and
 * move compares roles.target.
 */
export function commitmentAgendaEntry(view: ItemCommitmentView, ctx: { wsSlug: string; item: Item; collection: Collection | null; zone: string }): AgendaEntry | null {
 const next = view.next_occurrence;
 const collection = ctx.collection?.slug ?? ctx.item.collection_slug;
 if (!next || view.ended_at || view.error_code || !collection) return null;
 const target = view.roles.target;
 const scheduleZone = view.floating ? ctx.zone : view.tz ?? ctx.zone;
 const at = target ? (isDateOnly(target) ? wallToInstant(`${target}T00:00`, scheduleZone) : target) : next.at;
 const f = view.forecast;
 return {
  type: 'schedule', at, day: target ? roleDay(target, ctx.zone) : todayIn(ctx.zone, new Date(next.at)),
  wall: next.wall, state: next.state, item_ref: view.item_ref, item_title: ctx.item.title, collection, workspace: ctx.wsSlug,
  all_day: isDateOnly(target ?? view.dtstart), floating: view.floating, schedule_id: view.schedule_id, audience: view.audience,
  roles: view.roles, roles_origin: view.roles.roles_origin,
  ...(view.occurrence_key !== undefined ? { occurrence_key: view.occurrence_key } : {}),
  ...(view.snoozed_until !== undefined ? { snoozed_until: view.snoozed_until } : {}),
  ...(view.missed_count !== undefined ? { missed_count: view.missed_count } : {}),
  ...(view.stale !== undefined ? { stale: view.stale } : {}),
  ...(next.acknowledged_at !== undefined ? { acknowledged_at: next.acknowledged_at } : {}),
  ...(next.acknowledged_by !== undefined ? { acknowledged_by: next.acknowledged_by } : {}),
  ...(f ? { forecast: { value: f.value, set_by: f.set_by, ...(f.set_by_name !== undefined ? { set_by_name: f.set_by_name } : {}), set_by_kind: f.set_by_kind, set_at: f.set_at, source: f.source } } : {})
 };
}

/** Once plus the server's eight shorthands (internal/schedule/shorthand.go), as canonical rules. */
export const COMMITMENT_RECURRENCES = [
 ['once', undefined], ['daily', 'FREQ=DAILY'], ['weekdays', 'FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR'], ['weekly', 'FREQ=WEEKLY'],
 ['biweekly', 'FREQ=WEEKLY;INTERVAL=2'], ['monthly', 'FREQ=MONTHLY'], ['monthly-last', 'FREQ=MONTHLY;BYMONTHDAY=-1'],
 ['quarterly', 'FREQ=MONTHLY;INTERVAL=3'], ['yearly', 'FREQ=YEARLY']
] as const;
export type CommitmentRecurrence = typeof COMMITMENT_RECURRENCES[number][0];

/**
 * The POST body for a new commitment: zone-less walls, the target as the
 * date, and a hard deadline as a civil offset (calendar days plus a clock
 * difference), never elapsed milliseconds, so a DST change moves nothing.
 */
export function buildCommitmentCreate(input: {
 date: string; time?: string; rule: CommitmentRecurrence;
 deadline?: { date: string; time?: string; behavior: 'lapses' | 'stays_owed' };
}): CommitmentCreateRequest | { field: 'date' | 'time' | 'deadline'; message: string } {
 if (!isDateOnly(input.date)) return { field: 'date', message: 'Choose a target date.' };
 const timed = !!input.time;
 if (timed && !/^\d{2}:\d{2}$/.test(input.time!)) return { field: 'time', message: 'Choose a valid time.' };
 const rrule = COMMITMENT_RECURRENCES.find(([value]) => value === input.rule)?.[1];
 const body: CommitmentCreateRequest = { dtstart: timed ? `${input.date}T${input.time}` : input.date, ...(rrule ? { rrule } : {}), roles: { target_days: 0, target_secs: 0 } };
 const deadline = input.deadline;
 if (!deadline) return body;
 if (!isDateOnly(deadline.date)) return { field: 'deadline', message: 'Choose a deadline date.' };
 if (timed && !/^\d{2}:\d{2}$/.test(deadline.time ?? '')) return { field: 'deadline', message: 'Choose a deadline time.' };
 const day = (date: string) => Date.UTC(+date.slice(0, 4), +date.slice(5, 7) - 1, +date.slice(8, 10)) / 86_400_000;
 const clock = (time?: string) => timed && time ? +time.slice(0, 2) * 3600 + +time.slice(3, 5) * 60 : 0;
 const days = day(deadline.date) - day(input.date);
 const secs = clock(deadline.time) - clock(input.time);
 if (days < 0 || (days === 0 && secs < 0)) return { field: 'deadline', message: 'The deadline cannot be before the target.' };
 return { ...body, roles: { target_days: 0, target_secs: 0, deadline_days: days, deadline_secs: secs, deadline_behavior: deadline.behavior } };
}

type Behavior = 'lapses' | 'stays_owed';
const civilDay = (date: string) => Date.UTC(+date.slice(0, 4), +date.slice(5, 7) - 1, +date.slice(8, 10)) / 86_400_000;
const clockSecs = (time: string) => time ? +time.slice(0, 2) * 3600 + +time.slice(3, 5) * 60 + (time.length > 5 ? +time.slice(6, 8) : 0) : 0;

/** The wall a role offset lands on from a target date and clock ('' for all-day): calendar days, then a clock difference. */
export function offsetWall(date: string, time: string, days: number, secs: number): { date: string; time: string } {
 const at = new Date((civilDay(date) + days) * 86_400_000 + (clockSecs(time) + secs) * 1000).toISOString();
 return { date: at.slice(0, 10), time: time ? at.slice(11, 16) : '' };
}
/** The civil offset from a target to a wall: days between the dates plus the clock difference (DST-safe). */
export function wallOffset(date: string, time: string, toDate: string, toTime: string): { days: number; secs: number } {
 return { days: civilDay(toDate) - civilDay(date), secs: time ? clockSecs(toTime) - clockSecs(time) : 0 };
}

/** The Edit dialog's controls: the series start, its repeat and the hard deadline as a stored offset. */
export interface CommitmentEditForm {
 date: string;
 /** HH:MM, or '' for an all-day start. */
 time: string;
 /** null: a rule the shorthands cannot express (shown raw, read-only). */
 rule: CommitmentRecurrence | null;
 deadline: { days: number; secs: number; behavior: Behavior } | null;
}
export interface CommitmentEditState {
 initial: CommitmentEditForm;
 /** The stored start's seconds (":30"), kept unless the time is edited. */
 seconds: string;
 /** Why start and Repeats are read-only; '' when editable. */
 timingLock: string;
 /** Why the deadline is read-only; '' when editable. */
 deadlineLock: string;
 /** Other roles hang off the start, so adding or removing its time is refused. */
 timeLocked: boolean;
 /** A native row with a stored deadline: deadline edits go through renegotiate. */
 nativeDeadline: boolean;
 /** A timing edit re-plans from now and drops this occurrence's move or snooze. */
 replanDrops: boolean;
}

/** The Edit dialog's starting state, from the stored values only; null when the view offers no Edit. */
export function editStateFromView(view: ItemCommitmentView): CommitmentEditState | null {
 const o = view.role_offsets;
 if (!o || view.ended_at) return null;
 const [date, clock = ''] = view.dtstart.split('T');
 const time = clock.slice(0, 5);
 const rule = !view.rrule ? (view.rdates.length ? null : 'once') : COMMITMENT_RECURRENCES.find(([, r]) => r === view.rrule)?.[0] ?? null;
 const behavior = view.roles.deadline_behavior;
 const deadline = o.deadline_days !== null && o.deadline_secs !== null && behavior ? { days: o.deadline_days, secs: o.deadline_secs, behavior } : null;
 const nativeDeadline = !!deadline && view.roles.roles_origin === 'native';
 const linkedDeadline = view.links.some(l => (l.child_item_id === view.item_id && l.child_role === 'deadline') || (l.parent_item_id === view.item_id && l.parent_role === 'deadline'));
 const next = view.next_occurrence;
 const expected = next ? (isDateOnly(view.dtstart) ? next.wall.slice(0, 10) : next.at) : null;
 return {
  initial: { date, time, rule, deadline }, seconds: clock.slice(5),
  timingLock: nativeDeadline ? 'Fixed while a hard deadline exists; renegotiate or remove the deadline first.'
   : view.links.length ? 'Fixed while this date is linked to another item.'
   : rule === null || view.rdates.length || view.exdates.length ? `Repeats by a custom rule (${[view.rrule, view.rdates.length && 'additional dates', view.exdates.length && 'excluded dates'].filter(Boolean).join(', ')}); use pad remind edit to change it.`
   : o.target_days || o.target_secs ? 'Fixed because the target is offset from the start; use pad remind edit to change it.' : '',
  deadlineLock: linkedDeadline ? 'Fixed while the deadline is linked to another item.'
   : !time && deadline && deadline.secs ? 'This all-day date has a timed deadline; use pad remind edit to change it.' : '',
  timeLocked: o.earliest_days !== null || o.occupied_days !== null || !!deadline,
  nativeDeadline,
  replanDrops: !!view.next_override_at || !!view.snoozed_until || (!!expected && view.roles.target !== null && view.roles.target !== expected)
 };
}

export type CommitmentEditPlan =
 | { kind: 'none' }
 | { kind: 'patch'; body: CommitmentUpdateRequest }
 | { kind: 'renegotiate'; body: Pick<CommitmentRenegotiateRequest, 'scope' | 'deadline_offset' | 'deadline_behavior' | 'remove_deadline'> };

/**
 * The writes for an Edit save: only the members whose controls changed. A
 * native row's deadline goes through renegotiate alone (its timing is locked);
 * otherwise everything goes in one PATCH, which converts a legacy row.
 */
export function buildCommitmentEdit(state: CommitmentEditState, form: CommitmentEditForm, ctx: { zone: string; now: Date }): CommitmentEditPlan | { field: 'date' | 'time' | 'deadline'; message: string } {
 const init = state.initial;
 const timingChanged = form.date !== init.date || form.time !== init.time || form.rule !== init.rule;
 const d = form.deadline, i = init.deadline;
 const deadlineChanged = (d === null) !== (i === null) || (!!d && !!i && (d.days !== i.days || d.secs !== i.secs || d.behavior !== i.behavior));
 if (state.timingLock && timingChanged) return { field: 'date', message: state.timingLock };
 if (state.deadlineLock && deadlineChanged) return { field: 'deadline', message: state.deadlineLock };
 if (!isDateOnly(form.date)) return { field: 'date', message: 'Choose a target date.' };
 if (form.time && !/^\d{2}:\d{2}$/.test(form.time)) return { field: 'time', message: 'Choose a valid time.' };
 if (state.timeLocked && !form.time !== !init.time) return { field: 'time', message: 'The time can be added or removed only while the date has no other roles.' };
 if (d && (d.days < 0 || (d.days === 0 && d.secs < 0))) return { field: 'deadline', message: 'The deadline cannot be before the target.' };
 if (!form.time && d?.secs) return { field: 'deadline', message: 'An all-day date takes an all-day deadline.' };
 if (!timingChanged && !deadlineChanged) return { kind: 'none' };
 if (timingChanged && form.rule === 'once') {
  const passed = form.time ? Date.parse(wallToInstant(`${form.date}T${form.time}`, ctx.zone)) <= ctx.now.getTime() : form.date < todayIn(ctx.zone, ctx.now);
  if (passed) return { field: 'date', message: 'Choose a date that has not passed.' };
 }
 if (state.nativeDeadline) {
  if (!d) return { kind: 'renegotiate', body: { scope: 'series', remove_deadline: true } };
  const moved = d.days !== i!.days || d.secs !== i!.secs;
  return { kind: 'renegotiate', body: { scope: 'series', ...(moved ? { deadline_offset: { deadline_days: d.days, deadline_secs: d.secs } } : {}), ...(d.behavior !== i!.behavior ? { deadline_behavior: d.behavior } : {}) } };
 }
 const body: CommitmentUpdateRequest = {};
 if (form.date !== init.date || form.time !== init.time) body.dtstart = form.time ? `${form.date}T${form.time}${form.time === init.time ? state.seconds : ''}` : form.date;
 if (form.rule !== init.rule) body.rrule = COMMITMENT_RECURRENCES.find(([value]) => value === form.rule)?.[1] ?? null;
 if (deadlineChanged) body.roles = d ? { deadline_days: d.days, deadline_secs: d.secs, deadline_behavior: d.behavior } : { deadline_days: null, deadline_secs: null, deadline_behavior: null };
 return { kind: 'patch', body };
}

export function describeRecurrence(view: Pick<ItemCommitmentView, 'rrule' | 'rdates' | 'dtstart' | 'anchor'>): string {
 if (!view.rrule) return view.rdates.length ? 'specific dates' : 'once';
 // dtstart is a recurrence wall, not an instant in the browser's zone.
 const weekday = new Intl.DateTimeFormat('en-US', { weekday: 'long', timeZone: 'UTC' })
  .format(new Date(`${view.dtstart.slice(0, 10)}T12:00:00Z`));
 const rules: Record<string, [string, string]> = {
  'FREQ=DAILY': ['every day', '1 day'],
  'FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR': ['every weekday', 'every weekday'],
  'FREQ=WEEKLY': [`every ${weekday}`, '1 week'],
  'FREQ=WEEKLY;INTERVAL=2': [`every other ${weekday}`, '2 weeks'],
  'FREQ=MONTHLY': ['every month', '1 month'],
  'FREQ=MONTHLY;BYMONTHDAY=-1': ['last day of every month', 'last day of every month'],
  'FREQ=MONTHLY;INTERVAL=3': ['every 3 months', '3 months'],
  'FREQ=YEARLY': ['every year', '1 year']
 };
 const rule = rules[view.rrule];
 if (!rule) return view.anchor === 'completion' ? `${view.rrule} · after completion` : view.rrule;
 return view.anchor === 'completion' ? `${rule[1]} after completion` : rule[0];
}

export function commitmentDisplayState(view: ItemCommitmentView & { itemClosed?: boolean }, now: Date): {
 primary: string; annotations: string[]; mark?: OccurrenceState; unavailable: boolean;
 acknowledgedAt?: string; acknowledgedBy?: AcknowledgedBy;
} {
 const annotations: string[] = [];
 const unavailable = !!view.error_code || (!view.ended_at && !view.next_occurrence);
 const last = view.last_resolution?.state;
 const next = view.next_occurrence;
 let primary: string;
 let mark: OccurrenceState | undefined;
 if (unavailable) primary = 'Date unavailable';
 else if (view.ended_at) {
  primary = last === 'done' ? 'Done · schedule ended' : 'Ended';
  if (last === 'done') mark = 'done';
  if (last === 'skip') annotations.push('Last occurrence skipped');
 } else {
  const acknowledged = next?.state === 'acknowledged';
  const outstanding = acknowledged ? 'Outstanding · acknowledged' : 'Outstanding';
  const missed = view.missed_count ?? 0;
  const snoozed = !!view.snoozed_until && Date.parse(view.snoozed_until) > now.getTime();
  primary = snoozed ? 'Snoozed' : missed > 0 || view.stale ? 'Outstanding' : outstanding;
  mark = acknowledged ? 'acknowledged' : 'outstanding';
  if (snoozed || (acknowledged && primary !== outstanding)) annotations.push(outstanding);
  if (missed > 0) annotations.push(`Missed: ${view.missed_truncated ? 'at least ' : ''}${missed} later occurrences`);
  if (view.stale) annotations.push('Behind schedule');
  if (last === 'done' || last === 'skip') annotations.push(`Last occurrence ${last === 'done' ? 'done' : 'skipped'}`);
 }
 if (view.itemClosed) primary += ' · Item closed';
 return { primary, annotations, mark, unavailable,
  ...(!unavailable && !view.ended_at && next?.state === 'acknowledged'
   ? { acknowledgedAt: next.acknowledged_at, acknowledgedBy: next.acknowledged_by } : {}) };
}

export function describeLinkOffset(link: CommitmentLink, thisItemId: string, otherLabel: string): string {
 const role = (name: string) => name === 'occupied_until' ? 'occupied end' : name;
 const thisIsChild = link.child_item_id === thisItemId;
 const subject = thisIsChild ? `This item's ${role(link.child_role)} is` : `The ${role(link.child_role)} of ${otherLabel} is`;
 const object = thisIsChild ? `the ${role(link.parent_role)} of ${otherLabel}` : `this item's ${role(link.parent_role)}`;
 const offset = link.offset;
 if (offset.mode === 'elapsed') {
  const seconds = offset.elapsed_secs ?? 0;
  if (!seconds) return `${subject} at the same instant as ${object}`;
  let remainder = Math.abs(seconds);
  const duration: string[] = [];
  for (const [unit, size] of [['day', 86400], ['hour', 3600], ['minute', 60], ['second', 1]] as const) {
   const count = Math.floor(remainder / size);
   if (count) duration.push(`${count} ${unit}${count === 1 ? '' : 's'}`);
   remainder %= size;
  }
  // Elapsed time: a day here is 24 hours, not a calendar day.
  return `${subject} ${duration.join(' ')} ${seconds < 0 ? 'before' : 'after'} ${object} (elapsed time)`;
 }
 const days = offset.days ?? 0;
 const clock = offset.clock_secs ?? 0;
 const pad = (n: number) => String(n).padStart(2, '0');
 const time = `${pad(Math.floor(clock / 3600))}:${pad(Math.floor(clock % 3600 / 60))}${clock % 60 ? `:${pad(clock % 60)}` : ''}`;
 const when = offset.all_day ? `all day (${offset.zone})` : `at ${time} ${offset.zone}`;
 return days === 0 ? `${subject} on the same date as ${object}, ${when}`
  : `${subject} ${Math.abs(days)} calendar day${Math.abs(days) === 1 ? '' : 's'} ${days < 0 ? 'before' : 'after'} ${object}, ${when}`;
}

export function describeGate(gate: CommitmentGateView | CommitmentGate, otherLabel = 'Linked item'): string {
 if ('state' in gate) return { released: 'Released', waiting: 'Waiting', blocked_needs_decision: 'Blocked — decision needed' }[gate.state];
 if (gate.opened_at || gate.outcome === 'succeeded') return `Released by ${otherLabel}`;
 return gate.outcome === 'pending' ? `Waiting for ${otherLabel}` : `${otherLabel} ${gate.outcome} — decision needed`;
}
