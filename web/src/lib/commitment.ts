import type { CommitmentRoles } from '$lib/types';

export function commitmentKind(roles: Pick<CommitmentRoles, 'deadline' | 'occupied_until'>): 'deadline_hard' | 'window' | 'check' {
 if (roles.deadline !== null) return 'deadline_hard';
 if (roles.occupied_until !== null) return 'window';
 return 'check';
}

import type { AcknowledgedBy, CommitmentGate, CommitmentGateView, CommitmentLink, ItemCommitmentView } from '$lib/types';
import type { OccurrenceState } from '$lib/calendar/presentation';

/** undefined is loading/unavailable; null is an authoritative not_found. */
export interface ItemCommitmentSnapshot {
 wsSlug: string;
 itemId: string;
 itemSlug: string;
 authEpoch: number;
 zone: string;
 view: ItemCommitmentView | null | undefined;
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
