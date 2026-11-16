import type { Collection, CommitmentGate, CommitmentLink, Item, ItemCommitmentView } from '$lib/types';
export const item = { id: '11111111-1111-4111-8111-111111111111', slug: 'work', title: 'Work', collection_id: 'tasks-id', collection_slug: 'tasks', collection_prefix: 'TASK', item_number: 1, fields: '{"status":"open"}' } as Item;
export const collection = { id: 'tasks-id', slug: 'tasks', schema: JSON.stringify({ fields: [{ key: 'status', type: 'select', terminal_options: ['shipped', 'cancelled'], abandoned_options: ['cancelled'] }] }) } as Collection;
export const otherId = '22222222-2222-4222-8222-222222222222';
export function link(extra: Partial<CommitmentLink> = {}): CommitmentLink {
 return { id: 'link', parent_item_id: otherId, child_item_id: item.id, parent_role: 'target', child_role: 'deadline', offset: { mode: 'elapsed', elapsed_secs: 90061 }, cancel_policy: 'waiting_on', created_at: '', updated_at: '', ...extra };
}
export function gate(extra: Partial<CommitmentGate> = {}): CommitmentGate {
 return { id: 'gate', blocked_item_id: item.id, blocker_item_id: otherId, outcome: 'pending', created_at: '', ...extra };
}
export function view(extra: Partial<ItemCommitmentView> = {}): ItemCommitmentView {
 return {
  id: 'schedule', schedule_id: 'schedule', item_id: item.id, item_ref: 'TASK-1', dtstart: '2031-03-10T00:30:00', floating: false, tz: 'Pacific/Kiritimati', rrule: 'FREQ=WEEKLY', rdates: [], exdates: [], anchor: 'calendar', audience: 'both', lead_minutes: 0,
  roles: { target: '2031-03-10', earliest: '2031-03-09', deadline: '2031-03-11', deadline_behavior: 'stays_owed', occupied_until: '2031-03-12', outcome: 'pending', roles_origin: 'native' },
  forecast: { value: '2031-03-12', set_by: 'user', set_by_name: 'Alex', set_by_kind: 'user', set_at: '2031-03-01T09:00:00Z', source: 'Estimate', passed: false, history: [] },
  milestone: { children: [], counted: 0, total: 0, waivers: 0, result: null, evidence_changed: null, changes: [], evidence: [], history: [] },
  links: [], gates: { state: 'released', gates: [] }, journal: [], next_occurrence: { at: '2031-03-09T10:30:00Z', wall: '2031-03-10T00:30:00', state: 'outstanding' }, created_at: '', updated_at: '', ...extra
 };
}
