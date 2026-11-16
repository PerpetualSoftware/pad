import { describe, expect, it } from 'vitest';
import { buildCommitmentCreate, COMMITMENT_RECURRENCES, commitmentAgendaEntry, commitmentDisplayState, describeGate, describeLinkOffset, describeRecurrence } from './commitment';
import { collection, gate, item, link, otherId, view } from './test/commitmentFixtures';

const now = new Date('2031-03-10T12:00:00Z');
describe('recurrence', () => {
 it.each([
  ['FREQ=DAILY', 'every day', '1 day after completion'],
  ['FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR', 'every weekday', 'every weekday after completion'],
  ['FREQ=WEEKLY', 'every Monday', '1 week after completion'],
  ['FREQ=WEEKLY;INTERVAL=2', 'every other Monday', '2 weeks after completion'],
  ['FREQ=MONTHLY', 'every month', '1 month after completion'],
  ['FREQ=MONTHLY;BYMONTHDAY=-1', 'last day of every month', 'last day of every month after completion'],
  ['FREQ=MONTHLY;INTERVAL=3', 'every 3 months', '3 months after completion'],
  ['FREQ=YEARLY', 'every year', '1 year after completion']
 ])('%s preserves its cadence and completion anchor', (rrule, calendar, completion) => {
  expect(describeRecurrence(view({ rrule }))).toBe(calendar);
  expect(describeRecurrence(view({ rrule, anchor: 'completion' }))).toBe(completion);
 });
 it('uses the seed wall date, even when its instant is on Sunday', () => {
  expect(describeRecurrence(view())).toBe('every Monday');
  expect(describeRecurrence(view({ dtstart: '2031-03-10T00:30:00+14:00' }))).toBe('every Monday');
 });
 it('distinguishes once and specific dates and preserves unknown constraints', () => {
  expect(describeRecurrence(view({ rrule: undefined }))).toBe('once');
  expect(describeRecurrence(view({ rrule: undefined, rdates: ['2031-03-12'] }))).toBe('specific dates');
  const rrule = 'FREQ=WEEKLY;BYDAY=MO;COUNT=4';
  expect(describeRecurrence(view({ rrule }))).toBe(rrule);
 });
});
describe('display state', () => {
 it.each([
  [{ error_code: 'bad_rule' }, 'Date unavailable'],
  [{ next_occurrence: undefined }, 'Date unavailable'],
  [{ ended_at: '2031-03-09', last_resolution: { state: 'done', can_reopen: false } }, 'Done · schedule ended'],
  [{ ended_at: '2031-03-09' }, 'Ended'],
  [{ ended_at: '2031-03-09', last_resolution: { state: 'skip', can_reopen: false } }, 'Ended'],
  [{ snoozed_until: '2031-03-11T00:00:00Z', missed_count: 2 }, 'Snoozed'],
  [{ missed_count: 2 }, 'Outstanding'],
  [{ stale: true }, 'Outstanding'],
  [{ next_occurrence: { at: '', wall: '', state: 'acknowledged' as const } }, 'Outstanding · acknowledged'],
  [{}, 'Outstanding'],
  [{ snoozed_until: now.toISOString() }, 'Outstanding']
 ])('chooses primary for %j', (extra, primary) => {
  expect(commitmentDisplayState(view(extra), now).primary).toBe(primary);
 });
 it('keeps missed, acknowledged, stale and last-resolution annotations while snoozed', () => {
  const next_occurrence = { at: '', wall: '', state: 'acknowledged' as const, acknowledged_at: '2031-03-09T12:00:00Z', acknowledged_by: { user_id: 'user', name: 'Alex', actor: 'user' as const } };
  const state = commitmentDisplayState(view({ next_occurrence, snoozed_until: '2031-03-11', missed_count: 3, missed_truncated: true, stale: true, last_resolution: { state: 'done', can_reopen: false } }), now);
  expect(state.annotations).toEqual(expect.arrayContaining(['Outstanding · acknowledged', 'Missed: at least 3 later occurrences', 'Behind schedule', 'Last occurrence done']));
  expect(state.acknowledgedAt).toBe(next_occurrence.acknowledged_at);
  expect(state.acknowledgedBy).toEqual(next_occurrence.acknowledged_by);
 });
 it('does not invent counts or lapse history; labels closed items independently', () => {
  expect(commitmentDisplayState(view({ stale: true }), now).annotations).toEqual(['Behind schedule']);
  expect(commitmentDisplayState(view({ ended_at: '2031-03-09', last_resolution: { state: 'skip', can_reopen: false } }), now).annotations).toEqual(['Last occurrence skipped']);
  expect(commitmentDisplayState(view({ last_resolution: { state: 'skip', can_reopen: false } }), now).annotations).toContain('Last occurrence skipped');
  expect(commitmentDisplayState(view({ ended_at: '2031-03-09', last_resolution: { can_reopen: false } }), now).primary).toBe('Ended');
  expect(commitmentDisplayState({ ...view(), itemClosed: true }, now).primary).toBe('Outstanding · Item closed');
  expect(commitmentDisplayState(view({ roles: { ...view().roles, deadline: '2000-01-01', deadline_behavior: 'lapses' } }), now).primary).toBe('Outstanding');
 });
});
describe('directed links', () => {
 it.each([
  [{ mode: 'elapsed' as const, elapsed_secs: 90061 }, '1 day 1 hour 1 minute 1 second after', ' (elapsed time)'],
  [{ mode: 'elapsed' as const, elapsed_secs: -3600 }, '1 hour before', ' (elapsed time)'],
  [{ mode: 'elapsed' as const }, 'at the same instant as', ''],
  [{ mode: 'civil' as const, days: 2, clock_secs: 34215, zone: 'Europe/London' }, '2 calendar days after', ', at 09:30:15 Europe/London'],
  [{ mode: 'civil' as const, days: -1, clock_secs: 32400, zone: 'Europe/London' }, '1 calendar day before', ', at 09:00 Europe/London'],
  [{ mode: 'civil' as const, zone: 'UTC' }, 'on the same date as', ', at 00:00 UTC'],
  [{ mode: 'civil' as const, days: 1, all_day: true, zone: 'UTC' }, '1 calendar day after', ', all day (UTC)']
 ])('keeps both directions for %j', (offset, phrase, suffix) => {
  const l = link({ offset });
  expect(describeLinkOffset(l, item.id, 'TASK-2 · Prep')).toBe(`This item's deadline is ${phrase} the target of TASK-2 · Prep${suffix}`);
  expect(describeLinkOffset(l, otherId, 'TASK-1 · Work')).toBe(`The deadline of TASK-1 · Work is ${phrase} this item's target${suffix}`);
 });
 it('names earliest and occupied end', () => {
  expect(describeLinkOffset(link({ child_role: 'earliest', parent_role: 'occupied_until', offset: { mode: 'elapsed' } }), item.id, 'Prep')).toBe("This item's earliest is at the same instant as the occupied end of Prep");
 });
});
describe('incoming gates', () => {
 it.each([['released', 'Released'], ['waiting', 'Waiting'], ['blocked_needs_decision', 'Blocked — decision needed']] as const)('%s aggregate', (state, text) => {
  expect(describeGate({ state, gates: [] })).toBe(text);
 });
 it.each([['pending', 'Waiting for Prep'], ['succeeded', 'Released by Prep'], ['cancelled', 'Prep cancelled — decision needed'], ['lapsed', 'Prep lapsed — decision needed'], ['skipped', 'Prep skipped — decision needed']] as const)('%s blocker', (outcome, text) => {
  expect(describeGate(gate({ outcome }), 'Prep')).toBe(text);
 });
 it('opened_at is evidence of release', () => expect(describeGate(gate({ opened_at: '2031-03-09' }), 'Prep')).toBe('Released by Prep'));
});
describe('agenda entry adapter', () => {
 const ctx = { wsSlug: 'work', item, collection, zone: 'UTC' };
 it('maps the authoritative fields and omits what has no source', () => {
  const v = view({ occurrence_key: '2031-03-09T10:30:00Z|x', snoozed_until: '2031-03-11T00:00:00Z', missed_count: 1, stale: false,
   next_occurrence: { at: '2031-03-09T10:30:00Z', wall: '2031-03-10T00:30:00', state: 'acknowledged', acknowledged_at: '2031-03-09T11:00:00Z', acknowledged_by: { user_id: 'u', name: 'Alex', actor: 'user' } } });
  expect(commitmentAgendaEntry(v, ctx)).toEqual({
   type: 'schedule', at: '2031-03-09T10:00:00.000Z', day: '2031-03-10', wall: '2031-03-10T00:30:00', state: 'acknowledged',
   item_ref: 'TASK-1', item_title: 'Work', collection: 'tasks', workspace: 'work', all_day: true, floating: false,
   schedule_id: 'schedule', audience: 'both', roles: v.roles, roles_origin: 'native', occurrence_key: '2031-03-09T10:30:00Z|x',
   snoozed_until: '2031-03-11T00:00:00Z', missed_count: 1, stale: false,
   acknowledged_at: '2031-03-09T11:00:00Z', acknowledged_by: { user_id: 'u', name: 'Alex', actor: 'user' },
   forecast: { value: '2031-03-12', set_by: 'user', set_by_name: 'Alex', set_by_kind: 'user', set_at: '2031-03-01T09:00:00Z', source: 'Estimate' }
  });
 });
 it('preserves omission and never claims the reminder fired', () => {
  const entry = commitmentAgendaEntry(view({ forecast: null }), ctx)!;
  for (const key of ['fired', 'overdue', 'owner_username', 'workspace_name', 'snoozed_until', 'missed_count', 'stale', 'forecast', 'acknowledged_at', 'acknowledged_by', 'occurrence_key', 'agent_role_name', 'field', 'date', 'resolved_at', 'actor', 'comment_id']) expect(entry).not.toHaveProperty(key);
 });
 it('uses an instant target, or the next occurrence when there is no target', () => {
  expect(commitmentAgendaEntry(view({ roles: { ...view().roles, target: '2031-03-10T09:00:00Z' } }), ctx)).toMatchObject({ at: '2031-03-10T09:00:00Z', all_day: false, day: '2031-03-10' });
  expect(commitmentAgendaEntry(view({ roles: { ...view().roles, target: null } }), ctx)).toMatchObject({ at: '2031-03-09T10:30:00Z', all_day: false, day: '2031-03-09' });
 });
 it.each([[{ next_occurrence: undefined }], [{ ended_at: '2031-03-09' }], [{ error_code: 'bad_rule' }]])('returns null for %j', extra => {
  expect(commitmentAgendaEntry(view(extra), ctx)).toBeNull();
 });
});
describe('create body', () => {
 const keys = (body: object) => Object.keys(body).sort();
 it('sends a date-only once commitment with no rrule and no legacy members', () => {
  const body = buildCommitmentCreate({ date: '2031-03-10', time: '', rule: 'once' });
  expect(body).toEqual({ dtstart: '2031-03-10', roles: { target_days: 0, target_secs: 0 } });
  for (const key of ['kind', 'dtend', 'every', 'tz', 'floating', 'rrule']) expect(body).not.toHaveProperty(key);
 });
 it('sends a timed wall, never a UTC conversion', () => {
  expect(buildCommitmentCreate({ date: '2031-03-10', time: '09:30', rule: 'once' })).toEqual({ dtstart: '2031-03-10T09:30', roles: { target_days: 0, target_secs: 0 } });
 });
 it.each(COMMITMENT_RECURRENCES.filter(([value]) => value !== 'once'))('%s sends its canonical rule', (value, rrule) => {
  const body = buildCommitmentCreate({ date: '2031-03-10', rule: value });
  expect(body).toMatchObject({ rrule });
  expect(describeRecurrence({ rrule, rdates: [], dtstart: '2031-03-10', anchor: 'calendar' })).not.toBe(rrule);
 });
 it('counts deadline days on the calendar, so a DST change adds no seconds', () => {
  expect(buildCommitmentCreate({ date: '2031-03-08', time: '09:00', rule: 'once', deadline: { date: '2031-03-09', time: '09:00', behavior: 'lapses' } }))
   .toEqual({ dtstart: '2031-03-08T09:00', roles: { target_days: 0, target_secs: 0, deadline_days: 1, deadline_secs: 0, deadline_behavior: 'lapses' } });
  expect(buildCommitmentCreate({ date: '2031-03-08', rule: 'once', deadline: { date: '2031-04-08', behavior: 'stays_owed' } }))
   .toMatchObject({ roles: { deadline_days: 31, deadline_secs: 0, deadline_behavior: 'stays_owed' } });
 });
 it('keeps a signed clock difference for a timed deadline', () => {
  expect(buildCommitmentCreate({ date: '2031-03-10', time: '17:00', rule: 'once', deadline: { date: '2031-03-11', time: '09:00', behavior: 'lapses' } }))
   .toMatchObject({ roles: { deadline_days: 1, deadline_secs: -28800 } });
 });
 it('refuses a missing date, a deadline before the target and a timed deadline without a time', () => {
  expect(buildCommitmentCreate({ date: '', rule: 'once' })).toEqual({ field: 'date', message: 'Choose a target date.' });
  expect(buildCommitmentCreate({ date: '2031-03-10', rule: 'once', deadline: { date: '2031-03-09', behavior: 'lapses' } })).toEqual({ field: 'deadline', message: 'The deadline cannot be before the target.' });
  expect(buildCommitmentCreate({ date: '2031-03-10', time: '10:00', rule: 'once', deadline: { date: '2031-03-10', time: '09:59', behavior: 'lapses' } })).toEqual({ field: 'deadline', message: 'The deadline cannot be before the target.' });
  expect(buildCommitmentCreate({ date: '2031-03-10', time: '10:00', rule: 'once', deadline: { date: '2031-03-11', time: '', behavior: 'lapses' } })).toEqual({ field: 'deadline', message: 'Choose a deadline time.' });
  expect(keys(buildCommitmentCreate({ date: '2031-03-10', rule: 'once', deadline: { date: '2031-03-10', behavior: 'lapses' } }))).toEqual(['dtstart', 'roles']);
 });
});
