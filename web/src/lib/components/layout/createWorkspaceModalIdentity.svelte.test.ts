import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';

/**
 * BUG-2991 — the create-workspace modal's two post-await side effects.
 *
 * `create` and `import` both mint a workspace and then fire the Phase F
 * callback and NAVIGATE. If the signed-in user changed while the request was
 * open, that callback and that navigation belong to the session that ended:
 * the new user is sent to a slug that is not theirs, with the other account's
 * workspace name in the toast and the URL. The server denies them, so it is
 * never a bypass — it is a navigation nobody asked for.
 *
 * `create` is fenced in the STORE (it returns null); `import` calls the API
 * directly, so its fence lives in the component. Both are asserted HERE,
 * because the defect is in what the component does with the answer (CONVE-19)
 * — the store test cannot see a `goto`.
 */

const api = vi.hoisted(() => ({
	auth: { session: vi.fn() },
	templates: { list: vi.fn() },
	workspaces: {
		create: vi.fn(),
		importBundle: vi.fn(),
		list: vi.fn(),
		get: vi.fn(),
		me: vi.fn(),
	},
}));

const goto = vi.hoisted(() => vi.fn());

vi.mock('$lib/api/client', () => ({
	api,
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
}));
vi.mock('$app/navigation', () => ({ goto }));

import CreateWorkspaceModal from './CreateWorkspaceModal.svelte';
import { authStore } from '$lib/stores/auth.svelte';
import { uiStore } from '$lib/stores/ui.svelte';

const WS = { id: 'w1', slug: 'other', name: 'Other', owner_username: 'alice' };

function deferred<T>() {
	let resolve!: (v: T) => void;
	const promise = new Promise<T>((res) => { resolve = res; });
	return { promise, resolve };
}

/** Bounded microtask + effect flush. No timers, so nothing can hang on one. */
async function settle(): Promise<void> {
	for (let i = 0; i < 24; i++) {
		await Promise.resolve();
		await tick();
	}
}

function btn(container: HTMLElement, label: RegExp): HTMLButtonElement {
	const found = [...container.querySelectorAll('button')].find((b) =>
		label.test(b.textContent?.trim() ?? ''),
	);
	if (!found) throw new Error(`no button matching ${label}`);
	return found as HTMLButtonElement;
}

async function attachBundle(container: HTMLElement): Promise<void> {
	// Native `.click()` rather than `fireEvent.click`: Svelte 5 delegates
	// events from the mount root, and a synthetic MouseEvent dispatched at the
	// element did not reach the delegated handler here — the tab stayed on
	// `Create` and the import panel never rendered.
	btn(container, /^Import$/).click();
	await settle();
	const input = container.querySelector('input[type="file"]');
	if (!input) throw new Error('import panel did not render');
	const file = new File(['x'], 'bundle.tar.gz', { type: 'application/gzip' });
	Object.defineProperty(input, 'files', { value: [file], configurable: true });
	await fireEvent.input(input);
	await settle();
}

beforeEach(async () => {
	goto.mockClear();
	api.templates.list.mockResolvedValue([]);
	api.workspaces.list.mockResolvedValue([]);
	api.workspaces.get.mockResolvedValue(WS);
	api.workspaces.me.mockResolvedValue(null);
	api.workspaces.create.mockReset();
	api.workspaces.importBundle.mockReset();
	authStore.clear();
	api.auth.session.mockResolvedValue({
		authenticated: true,
		user: { id: 'u1', email: 'u1@example.com' },
	});
	await authStore.load();
	// The modal's `open` comes from the ui store, not a prop.
	uiStore.openCreateWorkspace();
});

afterEach(() => {
	cleanup();
	uiStore.closeCreateWorkspace();
	authStore.clear();
});

describe('the modal resets on the OPEN TRANSITION, not on every effect run', () => {
	it('keeps what the user typed when the templates response lands', async () => {
		// Found while writing the identity cases below: every click-driven test
		// failed because the reset effect re-ran and wiped the state under test.
		//
		// The effect READ `templates.length` and WROTE `templates` from its own
		// response, so it invalidated itself — the templates request it fires on
		// open came back a moment later and reset the typed name, the chosen
		// template, the expanded state and any selected bundle. Anyone typing
		// faster than that round-trip lost what they typed. It is also the shape
		// CONVE-1688 names, which in a production build can wedge the scheduler
		// rather than merely misbehaving.
		const templates = deferred<unknown[]>();
		api.templates.list.mockReturnValue(templates.promise);

		const { container } = render(CreateWorkspaceModal, { props: {} });
		const name = container.querySelector('#ws-create-name') as HTMLInputElement;
		await fireEvent.input(name, { target: { value: 'Typed before templates' } });
		await settle();

		// The response the effect asked for on open arrives now.
		templates.resolve([]);
		await settle();

		expect((container.querySelector('#ws-create-name') as HTMLInputElement).value)
			.toBe('Typed before templates');
	});

	it('keeps the Import tab selected when the templates response lands', async () => {
		const templates = deferred<unknown[]>();
		api.templates.list.mockReturnValue(templates.promise);

		const { container } = render(CreateWorkspaceModal, { props: {} });
		btn(container, /^Import$/).click();
		await settle();
		expect(container.querySelector('input[type="file"]')).not.toBeNull();

		templates.resolve([]);
		await settle();

		expect(container.querySelector('input[type="file"]')).not.toBeNull();
	});
});

describe('BUG-2991: the create-workspace modal does not act for a session that ended', () => {
	it('does not navigate when the user changes during a CREATE', async () => {
		const created = deferred<typeof WS>();
		api.workspaces.create.mockReturnValue(created.promise);
		const onWorkspaceCreated = vi.fn();

		const { container } = render(CreateWorkspaceModal, { props: { onWorkspaceCreated } });
		const name = container.querySelector('#ws-create-name') as HTMLInputElement;
		await fireEvent.input(name, { target: { value: 'Other' } });
		await fireEvent.click(btn(container, /Create Workspace/));

		// The POST is open. The session ends.
		authStore.clear();
		created.resolve(WS);
		await settle();

		expect(goto).not.toHaveBeenCalled();
		expect(onWorkspaceCreated).not.toHaveBeenCalled();
	});

	it('navigates when the user is unchanged during a CREATE', async () => {
		// The counterfactual: the fence must not make every create inert.
		api.workspaces.create.mockResolvedValue(WS);
		const onWorkspaceCreated = vi.fn();

		const { container } = render(CreateWorkspaceModal, { props: { onWorkspaceCreated } });
		const name = container.querySelector('#ws-create-name') as HTMLInputElement;
		await fireEvent.input(name, { target: { value: 'Other' } });
		await fireEvent.click(btn(container, /Create Workspace/));
		await settle();

		expect(goto).toHaveBeenCalledWith('/alice/other');
		expect(onWorkspaceCreated).toHaveBeenCalledWith(WS);
	});

	it('does not navigate when the user changes during an IMPORT', async () => {
		const imported = deferred<typeof WS>();
		api.workspaces.importBundle.mockReturnValue(imported.promise);
		const onWorkspaceCreated = vi.fn();

		const { container } = render(CreateWorkspaceModal, { props: { onWorkspaceCreated } });
		await attachBundle(container);
		await fireEvent.click(btn(container, /Import Workspace/));

		authStore.clear();
		imported.resolve(WS);
		await settle();

		expect(goto).not.toHaveBeenCalled();
		expect(onWorkspaceCreated).not.toHaveBeenCalled();
	});

	it('navigates when the user is unchanged during an IMPORT', async () => {
		api.workspaces.importBundle.mockResolvedValue(WS);
		const onWorkspaceCreated = vi.fn();

		const { container } = render(CreateWorkspaceModal, { props: { onWorkspaceCreated } });
		await attachBundle(container);
		await fireEvent.click(btn(container, /Import Workspace/));
		await settle();

		expect(goto).toHaveBeenCalledWith('/alice/other');
		expect(onWorkspaceCreated).toHaveBeenCalledWith(WS);
	});
});
