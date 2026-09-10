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

const toastShow = vi.hoisted(() => vi.fn());
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: toastShow, dismiss: vi.fn(), get toasts() { return []; } },
}));

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
	toastShow.mockClear();
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
		// NON-VACUITY, and it is not decoration: before BUG-3004 was fixed the
		// reset wiped the typed name, the submit button was disabled, the click
		// did nothing, and this exact assertion pair passed for that reason
		// (codex round 5 flagged the shape; the first version of this test had
		// it). "Did not navigate" is trivially true of a submit that never
		// happened.
		expect(api.workspaces.create).toHaveBeenCalled();

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

		// NON-VACUITY, same reason as the create case above.
		expect(api.workspaces.importBundle).toHaveBeenCalled();

		authStore.clear();
		imported.resolve(WS);
		await settle();

		expect(goto).not.toHaveBeenCalled();
		expect(onWorkspaceCreated).not.toHaveBeenCalled();
	});

	it('does not navigate when the user changes during the import\'s loadAll', async () => {
		// The SECOND await (codex round 5). One check after the upload is not
		// enough: `loadAll` is another await, and a swap landing inside it put
		// the callback, the toast and the navigation back in the new user's
		// session. The rule is per-await, not per-operation.
		api.workspaces.importBundle.mockResolvedValue(WS);
		const listing = deferred<unknown[]>();
		api.workspaces.list.mockReturnValue(listing.promise);
		const onWorkspaceCreated = vi.fn();

		const { container } = render(CreateWorkspaceModal, { props: { onWorkspaceCreated } });
		await attachBundle(container);
		btn(container, /Import Workspace/).click();
		await settle();

		expect(api.workspaces.importBundle).toHaveBeenCalled();
		// PRECONDITION, not decoration (codex round 8): without it a regression
		// that returned right after the FIRST identity check would satisfy every
		// assertion below, and this test exists for the SECOND one. Reaching
		// `api.workspaces.list` is what proves we are past the first check and
		// inside `loadAll`.
		expect(api.workspaces.list).toHaveBeenCalled();
		// The upload is done and the identity check after it has passed; we are
		// sitting inside `loadAll`.
		authStore.clear();
		listing.resolve([]);
		await settle();

		expect(goto).not.toHaveBeenCalled();
		expect(onWorkspaceCreated).not.toHaveBeenCalled();
	});

	it('does not report a FAILED import to the user who did not start it', async () => {
		// codex round 7. The success path was fenced and the failure path was
		// not, so an import that rejected after the signed-in user changed
		// showed A's error to B — an error for an operation B never started,
		// naming a file they never chose.
		const imported = deferred<typeof WS>();
		let rejectImport!: (e: unknown) => void;
		api.workspaces.importBundle.mockReturnValue(new Promise((_res, rej) => { rejectImport = rej; }));
		void imported;

		const { container } = render(CreateWorkspaceModal, { props: {} });
		await attachBundle(container);
		btn(container, /Import Workspace/).click();
		await settle();
		expect(api.workspaces.importBundle).toHaveBeenCalled();

		authStore.clear();
		rejectImport(new Error('boom'));
		await settle();

		expect(toastShow).not.toHaveBeenCalled();
	});

	it('DOES report a failed import to the user who started it', async () => {
		// The counterfactual: the fence must not swallow real errors.
		api.workspaces.importBundle.mockRejectedValue(new Error('boom'));

		const { container } = render(CreateWorkspaceModal, { props: {} });
		await attachBundle(container);
		btn(container, /Import Workspace/).click();
		await settle();

		expect(toastShow).toHaveBeenCalled();
		expect(String(toastShow.mock.calls[0]?.[0])).toContain('Import failed');
	});

	it('does not report a FAILED create to the user who did not start it', async () => {
		let rejectCreate!: (e: unknown) => void;
		api.workspaces.create.mockReturnValue(new Promise((_res, rej) => { rejectCreate = rej; }));

		const { container } = render(CreateWorkspaceModal, { props: {} });
		const name = container.querySelector('#ws-create-name') as HTMLInputElement;
		await fireEvent.input(name, { target: { value: 'Other' } });
		btn(container, /Create Workspace/).click();
		await settle();
		expect(api.workspaces.create).toHaveBeenCalled();

		authStore.clear();
		rejectCreate(new Error('boom'));
		await settle();

		expect(toastShow).not.toHaveBeenCalled();
	});

	it('closes itself when the signed-in user changes, so no draft is inherited', async () => {
		// codex round 7. The OPERATIONS were fenced; the DRAFT was not. A modal
		// left open by one user kept their typed name, description, chosen
		// template and selected bundle on screen for whoever signed in next —
		// visible to them, and submittable by them.
		const { container } = render(CreateWorkspaceModal, { props: {} });
		const name = container.querySelector('#ws-create-name') as HTMLInputElement;
		await fireEvent.input(name, { target: { value: 'A private project name' } });
		await settle();
		expect(uiStore.createWorkspaceOpen).toBe(true);

		authStore.clear();
		await settle();

		expect(uiStore.createWorkspaceOpen).toBe(false);
		// And reopening starts clean rather than restoring the draft.
		uiStore.openCreateWorkspace();
		await settle();
		expect((container.querySelector('#ws-create-name') as HTMLInputElement).value).toBe('');
	});

	it('does not close the NEXT user\'s freshly opened modal when a stale create lands', async () => {
		// codex round 8. `close()` is global — it writes
		// `uiStore.createWorkspaceOpen` — so a continuation belonging to the
		// previous session dismissed the modal the NEW user had just opened,
		// taking their draft with it. By then the identity listener has already
		// closed the previous user's modal, so there is nothing left for the
		// continuation to close.
		const created = deferred<typeof WS>();
		api.workspaces.create.mockReturnValue(created.promise);

		const { container } = render(CreateWorkspaceModal, { props: {} });
		const name = container.querySelector('#ws-create-name') as HTMLInputElement;
		await fireEvent.input(name, { target: { value: 'Other' } });
		btn(container, /Create Workspace/).click();
		await settle();
		expect(api.workspaces.create).toHaveBeenCalled();

		// Identity changes: the listener closes the modal.
		authStore.clear();
		await settle();
		expect(uiStore.createWorkspaceOpen).toBe(false);

		// The new user opens a fresh one...
		uiStore.openCreateWorkspace();
		await settle();
		expect(uiStore.createWorkspaceOpen).toBe(true);

		// ...and only THEN does the previous session's create land.
		created.resolve(WS);
		await settle();

		expect(uiStore.createWorkspaceOpen).toBe(true);
		expect(goto).not.toHaveBeenCalled();
	});

	it('does not re-enable the NEXT user\'s in-flight import when a stale one settles', async () => {
		// codex round 9. `importing` is COMPONENT state and the component
		// outlives the operation, so a stale import's `finally` cleared it
		// unconditionally: A starts an import, identity changes, B opens the
		// modal and starts their own, and A's promise settling re-enabled B's
		// button mid-upload — offering a duplicate submit.
		let resolveA!: (v: typeof WS) => void;
		api.workspaces.importBundle.mockReturnValueOnce(
			new Promise((res) => { resolveA = res; }),
		);

		const { container } = render(CreateWorkspaceModal, { props: {} });
		await attachBundle(container);
		btn(container, /Import Workspace/).click();
		await settle();
		expect(api.workspaces.importBundle).toHaveBeenCalledTimes(1);

		// Identity changes; the listener closes A's modal.
		authStore.clear();
		await settle();

		// B opens a fresh modal and starts their own import.
		uiStore.openCreateWorkspace();
		await settle();
		await attachBundle(container);
		let resolveB!: (v: typeof WS) => void;
		api.workspaces.importBundle.mockReturnValueOnce(
			new Promise((res) => { resolveB = res; }),
		);
		btn(container, /Import Workspace/).click();
		await settle();
		expect(api.workspaces.importBundle).toHaveBeenCalledTimes(2);

		// PRECONDITION: B's button really is disabled while B's upload runs.
		expect(btn(container, /Importing/).disabled).toBe(true);

		// Now A's stale import settles.
		resolveA(WS);
		await settle();

		// B's button must still be disabled — their upload is still running.
		expect(btn(container, /Importing/).disabled).toBe(true);

		resolveB(WS);
		await settle();
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
