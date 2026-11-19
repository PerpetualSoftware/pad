import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';

/** TASK-4643: the "give to…" picker on a needs-you row. */
const rolesList = vi.hoisted(() => vi.fn());
const membersList = vi.hoisted(() => vi.fn());

vi.mock('$lib/api/client', () => ({
	api: {
		agentRoles: { list: (ws: string) => rolesList(ws) },
		members: { list: (ws: string) => membersList(ws) }
	}
}));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: { userId: 'u-me' } }));

const { default: Picker } = await import('./NotMinePicker.svelte');

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

async function settle() {
	flushSync();
	for (let i = 0; i < 6; i++) await Promise.resolve();
	await tick();
	flushSync();
}

const member = (id: string, name: string) => ({ user_id: id, user_name: name, user_email: `${id}@x.test`, role: 'editor' });

async function mountPicker(onpick = vi.fn()) {
	app = mount(Picker, { target: host, props: { ws: 'alpha', onpick } }) as Record<string, unknown>;
	await settle();
	return onpick;
}

const rows = () => [...host.querySelectorAll('[role="menuitem"]')].map((b) => b.textContent?.replace(/\s+/g, ' ').trim());

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	rolesList.mockReset();
	membersList.mockReset();
});

afterEach(() => {
	if (app) unmount(app);
	app = null;
	host.remove();
});

describe('NotMinePicker', () => {
	it('lists roles, then people without the viewer, for the row workspace', async () => {
		rolesList.mockResolvedValue([
			{ id: 'r1', slug: 'reviewer', name: 'Reviewer', icon: '🔍' },
			{ id: 'r2', slug: 'planner', name: 'Planner', icon: '' }
		]);
		membersList.mockResolvedValue({ members: [member('u-me', 'Me Myself'), member('u-bob', 'Bob Person')], invitations: [] });
		await mountPicker();
		expect(rolesList).toHaveBeenCalledWith('alpha');
		expect(membersList).toHaveBeenCalledWith('alpha');
		expect(rows()).toEqual(['🔍 Reviewer', 'Planner', 'Bob Person']);
		expect([...host.querySelectorAll('.group-label')].map((h) => h.textContent?.trim())).toEqual(['Roles', 'People']);
	});

	it('a members 403 still shows the roles, and says people are unavailable', async () => {
		rolesList.mockResolvedValue([{ id: 'r1', slug: 'reviewer', name: 'Reviewer', icon: '' }]);
		membersList.mockRejectedValue(new Error('forbidden'));
		await mountPicker();
		expect(rows()).toEqual(['Reviewer']);
		expect(host.textContent).toContain('Members unavailable');
	});

	it('shows Loading until both answer, and an empty state when there is nobody to give to', async () => {
		let resolveRoles!: (v: unknown) => void;
		rolesList.mockImplementation(() => new Promise((r) => (resolveRoles = r)));
		membersList.mockResolvedValue({ members: [member('u-me', 'Me Myself')], invitations: [] });
		await mountPicker();
		expect(host.textContent).toContain('Loading');
		resolveRoles([]);
		await settle();
		expect(host.textContent).not.toContain('Loading');
		expect(host.textContent).toContain('Nobody else to give it to');
	});

	it('picking a row calls onpick with kind, id and label', async () => {
		rolesList.mockResolvedValue([{ id: 'r1', slug: 'reviewer', name: 'Reviewer', icon: '' }]);
		membersList.mockResolvedValue({ members: [member('u-bob', 'Bob Person')], invitations: [] });
		const onpick = await mountPicker();
		const buttons = [...host.querySelectorAll<HTMLButtonElement>('[role="menuitem"]')];
		buttons[0].click();
		buttons[1].click();
		expect(onpick.mock.calls).toEqual([
			[{ kind: 'role', id: 'r1', label: 'Reviewer' }],
			[{ kind: 'user', id: 'u-bob', label: 'Bob Person' }]
		]);
	});
});
