import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';

/**
 * TASK-2759, the worst mis-attribution the surface enumeration found.
 *
 * An agent authenticates with a person's credentials, so this column's
 * `actor_name` — joined from `user_id` — is that person. It was checked
 * FIRST, so an agent's write rendered under a human's name on the one
 * surface that exists to answer "who did this", with nothing anywhere in the
 * row to say an agent was involved.
 *
 * The fix renders both facts rather than swapping which one is hidden, and
 * these assert the page's rendered cell (CONVE-19): `agentActor.ts` is unit
 * tested separately, and a correct helper this page never calls would pass
 * all of those.
 */
const adminFetchMock = vi.hoisted(() => vi.fn<(path: string) => Promise<unknown>>());

vi.mock('$lib/stores/admin.svelte', () => ({
	adminFetch: (path: string) => adminFetchMock(path)
}));

const { default: AuditLogPage } = await import('./audit-log/+page.svelte');

interface Row {
	id: string;
	action: string;
	actor: string;
	source: string;
	created_at: string;
	metadata?: string;
	actor_name?: string;
	user_id?: string;
}

function row(over: Partial<Row> = {}): Row {
	return {
		id: 'a1',
		action: 'updated',
		actor: 'agent',
		source: 'cli',
		created_at: new Date('2026-08-24T12:00:00Z').toISOString(),
		metadata: JSON.stringify({ agent: 'wren' }),
		actor_name: 'Dave',
		user_id: 'user-1',
		...over
	};
}

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

/** Mount the page with `rows` as the audit-log response and let its
 *  onMount fetch resolve into the table. */
async function mountWith(rows: Row[]): Promise<void> {
	adminFetchMock.mockResolvedValue(rows);
	app = mount(AuditLogPage, { target: host, props: {} }) as Record<string, unknown>;
	flushSync();
	await Promise.resolve();
	await Promise.resolve();
	await tick();
	flushSync();
}

/** The user column is the second cell of each body row. */
function userCells(): string[] {
	return [...host.querySelectorAll('tbody tr')].map(
		(tr) => tr.querySelectorAll('td')[1]?.textContent?.trim() ?? ''
	);
}

/** The details column — fourth cell, after time / user / action. */
function detailCells(): string[] {
	return [...host.querySelectorAll('tbody tr')].map(
		(tr) => tr.querySelectorAll('td')[3]?.textContent?.trim() ?? ''
	);
}

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	adminFetchMock.mockReset();
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
});

describe('console audit log — agent attribution', () => {
	it('names the agent and the account it acted as', async () => {
		await mountWith([row()]);

		expect(userCells()).toEqual(['wren (via Dave)']);
	});

	it('does not render the human alone for an agent write', async () => {
		// The counterfactual, stated as its own case: this is exactly what the
		// pre-TASK-2759 cell produced, and it is what the cell produces again
		// if the agent branch is dropped or ordered after the actor_name one.
		await mountWith([row()]);

		expect(userCells()).not.toEqual(['Dave']);
	});

	// Codex round 8. The agent half of this cell is attacker-chosen text and
	// the account half is not, so the two must not be concatenated into one
	// string: a writer could pick a name that forges the "(via …)" suffix, or
	// that carries U+202E and visually reorders the suffix appended after it.
	// The audited party would then be editing how the audit reads.
	it('keeps a forged "(via …)" inside the self-declared half', async () => {
		await mountWith([row({ metadata: JSON.stringify({ agent: 'admin (via root)' }) })]);

		// Exactly one real "via" element, and it names the actual account.
		const via = host.querySelectorAll('.user-cell .via');
		expect(via).toHaveLength(1);
		expect(via[0].textContent!.trim()).toBe('(via Dave)');
		// The forgery is text inside the agent's own element, not structure.
		expect(host.querySelector('.user-cell .agent-name')!.textContent).toBe('admin (via root)');
	});

	it('isolates each name so a bidi control cannot reorder its neighbours', async () => {
		// U+202E RIGHT-TO-LEFT OVERRIDE reorders everything after it until the
		// end of its isolate. <bdi> is what makes "until the end" mean "this
		// name", rather than the rest of the cell.
		await mountWith([row({ metadata: JSON.stringify({ agent: 'wren‮gnimalb' }) })]);

		const agentEl = host.querySelector('.user-cell .agent-name')!;
		expect(agentEl.tagName).toBe('BDI');
		expect(host.querySelector('.user-cell .via bdi')!.tagName).toBe('BDI');
		// The account name is in its own isolate, so it is still a whole,
		// separately-ordered value no matter what the agent name contains.
		expect(host.querySelector('.user-cell .via bdi')!.textContent).toBe('Dave');
	});

	it('renders a generic-looking client id verbatim', async () => {
		//'wren' alone does not discriminate: reinstating the retired
		// GENERIC_AGENT_IDS filter left this file green until this case
		// existed, because no fixture used a value the filter would swallow.
		await mountWith([row({ metadata: JSON.stringify({ agent: 'claude-code' }) })]);

		expect(userCells()).toEqual(['claude-code (via Dave)']);
	});

	it('renders the agent alone when no account name resolved', async () => {
		await mountWith([row({ actor_name: undefined })]);

		expect(userCells()).toEqual(['wren']);
	});

	it.each([
		['no agent key', JSON.stringify({ ip: '127.0.0.1' })],
		['an empty name', JSON.stringify({ agent: '' })],
		['unparseable metadata', 'not json'],
		['absent metadata', undefined]
	])('falls back to the account name given %s', async (_case, metadata) => {
		await mountWith([row({ metadata })]);

		expect(userCells()).toEqual(['Dave']);
	});

	it('ignores a stamp on a row whose actor is not an agent', async () => {
		// The metadata blob is shared, and agentMeta merges into it by string
		// splice — an `agent` key on a human's row is not a claim that an
		// agent acted, and reading it unconditionally would invent one.
		await mountWith([row({ actor: 'user' })]);

		expect(userCells()).toEqual(['Dave']);
	});

	// Codex round 6. Hoisting the row's parse out of formatMetadata left its
	// try/catch wrapped around nothing, so a formatter that throws on
	// well-formed JSON — `String(data.keys)` cannot convert `{"toString":null}`
	// to a primitive — would take the whole page down instead of rendering an
	// em dash. These drive the Details column, which no test here touched, and
	// they fail against the narrowed guard.
	it('renders an em dash when a formatter throws on well-formed metadata', async () => {
		await mountWith([
			row({
				actor: 'user',
				action: 'settings_changed',
				metadata: '{"keys":{"toString":null}}'
			})
		]);

		expect(detailCells()).toEqual(['—']);
		// The row still rendered at all — the counterfactual is a blank table.
		expect(userCells()).toEqual(['Dave']);
	});

	it('formats a known action from the shared parse', async () => {
		// Proves the hoisted object actually reaches formatMetadata, not just
		// displayUser: a hoist that passed the wrong value would em-dash here.
		await mountWith([
			row({
				actor: 'user',
				action: 'role_changed',
				metadata: JSON.stringify({ old_role: 'editor', new_role: 'admin' })
			})
		]);

		expect(detailCells()).toEqual(['editor → admin']);
	});

	it('leaves the existing System and user-id fallbacks intact', async () => {
		await mountWith([
			row({ id: 'a2', actor: 'system', actor_name: undefined, metadata: '{}' }),
			row({
				id: 'a3',
				actor: 'user',
				actor_name: undefined,
				user_id: 'user-abcdefghijklmnop',
				metadata: '{}'
			})
		]);

		// 12 characters then an ellipsis, per the column's existing truncation.
		expect(userCells()).toEqual(['System', 'user-abcdefg…']);
	});
});
