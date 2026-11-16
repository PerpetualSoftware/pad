import { describe, expect, it } from 'vitest';
import { commitmentDisplayState, describeGate, describeLinkOffset, describeRecurrence } from './commitment';
import { gate, item, link, otherId, view } from './test/commitmentFixtures';

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
  [{ mode: 'elapsed' as const, elapsed_secs: 90061 }, '1 day 1 hour 1 minute 1 second elapsed after', ' (1 day = 24 hours)'],
  [{ mode: 'elapsed' as const, elapsed_secs: -3600 }, '1 hour elapsed before', ' (1 day = 24 hours)'],
  [{ mode: 'elapsed' as const }, 'at the same instant as', ''],
  [{ mode: 'civil' as const, days: 2, clock_secs: 34215, zone: 'Europe/London' }, '2 calendar days after', "'s date at 09:30:15 Europe/London"],
  [{ mode: 'civil' as const, days: -1, clock_secs: 32400, zone: 'Europe/London' }, '1 calendar day before', "'s date at 09:00 Europe/London"],
  [{ mode: 'civil' as const, zone: 'UTC' }, 'on the same date as', " at 00:00 UTC"],
  [{ mode: 'civil' as const, days: 1, all_day: true, zone: 'UTC' }, '1 calendar day after', "'s date all day (UTC)"]
 ])('keeps both directions for %j', (offset, phrase, suffix) => {
  const l = link({ offset });
  expect(describeLinkOffset(l, item.id, 'TASK-2 · Prep')).toBe(`This item's deadline is ${phrase} TASK-2 · Prep's target${suffix}`);
  expect(describeLinkOffset(l, otherId, 'TASK-1 · Work')).toBe(`TASK-1 · Work's deadline is ${phrase} this item's target${suffix}`);
 });
 it('names earliest and occupied end', () => {
  expect(describeLinkOffset(link({ child_role: 'earliest', parent_role: 'occupied_until', offset: { mode: 'elapsed' } }), item.id, 'Prep')).toBe("This item's earliest is at the same instant as Prep's occupied end");
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
