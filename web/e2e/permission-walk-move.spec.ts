import { test, expect, request, type Locator, type Page } from '@playwright/test';
import {
	actAs,
	seedPermissionWalk,
	waitForAccessSettled,
	type PermissionWalk
} from './lib/permission-walk';

/**
 * Permission walk, the doors that MOVE a card (BUG-3259). Moving writes the
 * card's status, sort_order or role, and each door used to gate on the
 * collection (or the parent, or "any edit grant") instead of on the card.
 *
 * The account is the precedence guest: it may edit Tasks through a collection
 * grant, but task A only through an item-VIEW grant, which wins. So A is
 * view-only inside a collection the account may edit, and every door must
 * refuse to move A while still moving C, which the account may edit. C is the
 * in-test control on every leg: an absence on A is evidence only beside the
 * same gesture working on C.
 *
 * Every leg also records the item writes the page sent, and asserts none was
 * refused: a 403 is the client sending a write it cannot make.
 */

test.skip(({ isMobile }) => isMobile, 'the permission walk runs on the desktop project');

let walk: PermissionWalk;
// Item ids, to read which card a write names.
const ids: Record<'A' | 'C', string> = { A: '', C: '' };
const idByTitle = new Map<string, string>();

test.beforeAll(async () => {
	walk = await seedPermissionWalk();
	const owner = await request.newContext({
		baseURL: walk.baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${walk.accounts.owner.token}` }
	});
	const ws = `/api/v1/workspaces/${walk.workspaceSlug}`;
	// Door 7: C is the parent of A (view-only) and B (editable).
	for (const child of [walk.grantedTask, walk.tasks[1]]) {
		const r = await owner.patch(`${ws}/items/${child.slug}`, { data: { fields_patch: { parent: walk.tasks[2].ref } } });
		expect(r.status(), `parent ${child.ref}`).toBe(200);
	}
	// Door 8: A and F hold a role, so both sit in a role lane. F is the roles
	// legs' own editable card: no other leg moves it, so a board leg cannot
	// move it to a status the roles board does not show.
	const role = await owner.post(`${ws}/agent-roles`, { data: { name: 'Builder' } });
	expect(role.status()).toBe(201);
	const { id: roleId } = (await role.json()) as { id: string };
	const f = await owner.post(`${ws}/collections/tasks/items`, { data: { title: F_TITLE } });
	expect(f.status(), 'create F').toBe(201);
	const fItem = (await f.json()) as { id: string; slug: string };
	idByTitle.set(F_TITLE, fItem.id);
	for (const slug of [walk.grantedTask.slug, fItem.slug]) {
		const r = await owner.patch(`${ws}/items/${slug}`, { data: { agent_role_id: roleId } });
		expect(r.status(), `role ${slug}`).toBe(200);
	}
	for (const it of [walk.grantedTask, ...walk.tasks]) {
		const r = await owner.get(`${ws}/items/${it.slug}`);
		idByTitle.set(it.title, ((await r.json()) as { id: string }).id);
	}
	// Two more editable tasks, so the legs that pick an editable mover always
	// have one beside A, with B reserved for the child legs.
	for (const title of ['Walk move D', 'Walk move E']) {
		const r = await owner.post(`${ws}/collections/tasks/items`, { data: { title } });
		expect(r.status(), `create ${title}`).toBe(201);
		idByTitle.set(title, ((await r.json()) as { id: string }).id);
	}
	ids.A = idByTitle.get(walk.grantedTask.title)!;
	ids.C = idByTitle.get(walk.tasks[2].title)!;
	await owner.dispose();
});

const A = () => walk.grantedTask.title;
const C = () => walk.tasks[2].title;
// B is C's other child. The child-list legs need it beside A (same status
// group), so no other leg picks it as a card to move.
const B = () => walk.tasks[1].title;
const F_TITLE = 'Walk roles F';
const F = () => F_TITLE;
const movable = (t: string, allowB = false) => t !== A() && t !== F() && (allowB || t !== B());

async function open(page: Page, path: string, view?: 'board' | 'list' | 'table') {
	await actAs(page.context(), walk.accounts.guestPrecedence);
	if (view) {
		await page.context().addInitScript((v) => {
			try {
				localStorage.setItem('pad-view-tasks', v);
			} catch {
				// storage unavailable: the collection's default view applies
			}
		}, view);
	}
	await page.goto(`${walk.workspacePath}${path}`);
	await waitForAccessSettled(page);
}

// Every write the page sends to the API: item PATCHes, and the lane reorder
// batches (the roles board PUTs role_sort_order for a whole lane, and the
// server refuses the batch if it names a card the caller may only view).
function recordWrites(page: Page): string[] {
	const writes: string[] = [];
	page.on('response', (r) => {
		const method = r.request().method();
		const path = new URL(r.url()).pathname;
		if (['PATCH', 'PUT', 'DELETE'].includes(method) && path.startsWith('/api/v1/')) {
			writes.push(`${r.status()} ${method} ${path} ${r.request().postData() ?? ''}`);
		}
	});
	return writes;
}

const refused = (writes: string[]) => writes.filter((w) => w.startsWith('403 '));

/**
 * An editable card (any task but A) that sits in a zone with a neighbour, and
 * the arrow key that moves it one place: a keyboard move that must COMPLETE,
 * so a control proves more than that the drag starts. Legs share one world
 * per worker, so the card is chosen from what is on screen now.
 */
async function keyboardControl(page: Page, zoneSel: string, titleSel: string, allowB = false): Promise<{ title: string; step: 'ArrowUp' | 'ArrowDown' }> {
	const zones = page.locator(zoneSel);
	for (let z = 0; z < (await zones.count()); z++) {
		const titles = (await zones.nth(z).locator(titleSel).allTextContents()).map((t) => t.trim());
		if (titles.length < 2) continue;
		const i = titles.findIndex((t) => movable(t, allowB));
		if (i >= 0) return { title: titles[i], step: i > 0 ? 'ArrowUp' : 'ArrowDown' };
	}
	throw new Error(`no zone matching ${zoneSel} holds an editable card with a neighbour`);
}

/** The keyboard move completed and persisted: a 200 sort_order write after
 *  `before`. Not necessarily naming the mover: with a view-only card in the
 *  zone the plan may keep the mover's value and renumber a neighbour. */
async function expectMoved(writes: string[], before: number): Promise<void> {
	await expect
		.poll(() => writes.slice(before).some((w) => w.startsWith('200 ') && w.includes('sort_order')), { timeout: 5_000 })
		.toBe(true);
}

/** `order` with `mover` swapped one place toward `dir` (-1 up, +1 down). */
function swapWith(order: string[], mover: string, dir: -1 | 1): string[] {
	const i = order.indexOf(mover);
	const j = i + dir;
	if (i < 0 || j < 0 || j >= order.length) return order;
	const next = [...order];
	[next[i], next[j]] = [next[j], next[i]];
	return next;
}

/**
 * Open a card's action menu and click one of its Move entries, failing in
 * seconds with what the menu actually rendered (BUG-3278). The menu HIDES an
 * entry its position rules out (disabledDirections), so a bare getByRole click
 * on an absent entry waits out the whole test budget and reports only the
 * name it waited for. `where` describes the list the entry was chosen from.
 */
async function clickMenuEntry(page: Page, trigger: Locator, entry: Locator, where: () => Promise<string>): Promise<void> {
	await trigger.click();
	await expect(trigger, 'the card menu opened').toHaveAttribute('aria-expanded', 'true', { timeout: 5_000 });
	try {
		await expect(entry.first()).toBeVisible({ timeout: 5_000 });
	} catch {
		const rendered = (await page.getByRole('menuitem').allTextContents()).map((t) => t.trim());
		throw new Error(`menu entry ${entry} not rendered; the open menu has [${rendered.join(', ')}]; ${await where()}`);
	}
	await entry.first().click();
}

/** GET a workspace path as the owner, to read stored state the page does not show. */
async function ownerRead(path: string): Promise<unknown> {
	const owner = await request.newContext({
		baseURL: walk.baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${walk.accounts.owner.token}` }
	});
	try {
		return await (await owner.get(`/api/v1/workspaces/${walk.workspaceSlug}${path}`)).json();
	} finally {
		await owner.dispose();
	}
}

/** `container` elements whose own `titleSel` element reads exactly `title`.
 *  A card also shows its PARENT's title in a chip, so hasText would match the
 *  children of C as C. */
function titled(page: Page, container: string, titleSel: string, title: string): Locator {
	const esc = title.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
	return page.locator(container, { has: page.locator(titleSel, { hasText: new RegExp(`^\\s*${esc}\\s*$`) }) });
}

/** Whether a pointer drag of `el` starts within three attempts. A gesture can
 *  fail to start under load (the -collections drag helper retries for the
 *  same reason), so a control gets three tries and a locked card must refuse
 *  all three. */
async function pointerEngages(page: Page, el: Locator): Promise<boolean> {
	for (let attempt = 0; attempt < 3; attempt++) {
		if (await pointerEngagesOnce(page, el)) return true;
	}
	return false;
}

/** Press on `el`, move far enough to start a drag, and report whether the
 *  library mounted its dragged element. Releases back where it started. */
async function pointerEngagesOnce(page: Page, el: Locator): Promise<boolean> {
	await expect(el).toBeVisible();
	// A press needs the card on screen; a long lane pushes it below the fold.
	await el.scrollIntoViewIfNeeded();
	const b = (await el.boundingBox())!;
	const x = b.x + b.width / 2;
	const y = b.y + b.height / 2;
	await page.mouse.move(x, y);
	await page.mouse.down();
	await page.mouse.move(x + 12, y + 12, { steps: 4 });
	await page.mouse.move(x + 30, y + 30, { steps: 4 });
	const engaged = (await page.locator('#dnd-action-dragged-el').count()) > 0;
	// Release OFF the card, in the top bar, outside every drop zone. On a card
	// that did not start a drag, a press and release on it is a click, which
	// opens the item (the roles board's cards are links). A drag released
	// outside every zone returns the card to where it started.
	await page.mouse.move(page.viewportSize()!.width / 2, 8, { steps: 4 });
	await page.mouse.up();
	// Let a drop settle before the next gesture.
	await expect(page.locator('#dnd-action-dragged-el')).toHaveCount(0);
	return engaged;
}

/** Focus `el` (a zone child) and press Space: whether a keyboard drag began.
 *  With `step`, the lifted card moves one position before it is dropped. */
async function keyboardEngages(page: Page, el: Locator, step?: 'ArrowUp' | 'ArrowDown'): Promise<boolean> {
	await el.focus();
	await page.keyboard.press(' ');
	const alert = page.locator('#dnd-action-aria-alert');
	const started = await expect(alert)
		.toContainText('Started dragging', { timeout: 1_000 })
		.then(() => true)
		.catch(() => false);
	// Optionally move it one step, then drop it.
	if (started && step) await page.keyboard.press(step);
	if (started) await page.keyboard.press(' ');
	return started;
}

test.describe('guestPrecedence: a view-only card does not move (BUG-3259)', () => {
	test('board: pointer drag', async ({ page }) => {
		const writes = recordWrites(page);
		await open(page, '/tasks', 'board');
		const card = (t: string) => titled(page, '.column-cards > .card-wrapper', '.card-title', t);
		expect(await pointerEngages(page, card(C())), 'control: C drags').toBe(true);
		expect(await pointerEngages(page, card(A())), 'A must not drag').toBe(false);
		expect(refused(writes)).toEqual([]);
	});

	test('board: keyboard drag (Space on the focused card)', async ({ page }) => {
		const writes = recordWrites(page);
		await open(page, '/tasks', 'board');
		const card = (t: string) => titled(page, '.column-cards > .card-wrapper', '.card-title', t);
		await expect(card(A())).toBeVisible();
		expect(await keyboardEngages(page, card(A())), 'A must not lift by keyboard').toBe(false);
		const ctl = await keyboardControl(page, '.column-cards', '.card-title');
		const before = writes.length;
		expect(await keyboardEngages(page, card(ctl.title), ctl.step), 'control: an editable card lifts and moves by keyboard').toBe(true);
		await expectMoved(writes, before);
		expect(refused(writes)).toEqual([]);
	});

	test('list: pointer drag', async ({ page }) => {
		const writes = recordWrites(page);
		await open(page, '/tasks', 'list');
		const row = (t: string) => titled(page, '.group-items > .list-row', '.card-title', t);
		expect(await pointerEngages(page, row(C())), 'control: C drags').toBe(true);
		expect(await pointerEngages(page, row(A())), 'A must not drag').toBe(false);
		expect(refused(writes)).toEqual([]);
	});

	test('list: keyboard drag (Space on the focused row)', async ({ page }) => {
		const writes = recordWrites(page);
		await open(page, '/tasks', 'list');
		const row = (t: string) => titled(page, '.group-items > .list-row', '.card-title', t);
		await expect(row(A())).toBeVisible();
		expect(await keyboardEngages(page, row(A())), 'A must not lift by keyboard').toBe(false);
		const ctl = await keyboardControl(page, '.group-items', '.card-title');
		const before = writes.length;
		expect(await keyboardEngages(page, row(ctl.title), ctl.step), 'control: an editable row lifts and moves by keyboard').toBe(true);
		await expectMoved(writes, before);
		expect(refused(writes)).toEqual([]);
	});

	for (const view of ['board', 'list', 'table'] as const) {
		test(`${view}: the card menu's Move entries`, async ({ page }) => {
			const writes = recordWrites(page);
			await open(page, '/tasks', view);
			const holder = (t: string) =>
				view === 'table'
					? titled(page, '[role=row]', '.title-link', t)
					: titled(page, '.item-card', '.card-title', t);
			const titleSel = view === 'table' ? '.title-link' : '.card-title';
			await expect(holder(A())).toBeVisible();
			// The control is an editable card in A's own lane or group, so it has
			// somewhere to move whatever earlier legs in this world did. Every
			// task but A is editable for this account.
			const scope =
				view === 'board'
					? page.locator('.column-cards', { has: page.locator('.card-title', { hasText: new RegExp(`^\\s*${A()}\\s*$`) }) })
					: view === 'list'
						? page.locator('.group-items', { has: page.locator('.card-title', { hasText: new RegExp(`^\\s*${A()}\\s*$`) }) })
						: page.locator('body');
			const peers = (await scope.locator(titleSel).allTextContents()).map((t) => t.trim()).filter((t) => movable(t));
			expect(peers.length, 'A has an editable neighbour to use as the control').toBeGreaterThan(0);
			const control = peers[0];
			await expect(holder(control).locator('.iam-trigger'), 'control: an editable card has the menu').toHaveCount(1);
			await expect(holder(A()).locator('.iam-trigger'), 'A has no move menu').toHaveCount(0);
			// The control's menu still MOVES it: a named Move entry, and a write
			// that names the control lands. Nothing sent is refused.
			const move = view === 'board' ? /Move (right|left)/ : /Move (down|up|to top|to bottom)/;
			await clickMenuEntry(
				page,
				holder(control).locator('.iam-trigger'),
				page.getByRole('menuitem', { name: move }).and(page.locator(':not([disabled])')),
				async () => `control ${control} among ${peers.join(', ')}`
			);
			const controlId = idByTitle.get(control)!;
			await expect
				.poll(() => writes.some((w) => w.startsWith('200 ') && w.includes(controlId)), { timeout: 5_000 })
				.toBe(true);
			expect(refused(writes)).toEqual([]);
		});
	}

	test('board: moving an editable card past a view-only neighbour writes nothing refused', async ({ page }) => {
		const writes = recordWrites(page);
		await open(page, '/tasks', 'board');
		// Put C in A's lane, directly above it, through the card menu: the lane
		// reorder then covers A, which it used to renumber (a 403).
		const lanes = page.locator('.column-cards');
		const laneOf = async (t: string) => {
			for (let i = 0; i < (await lanes.count()); i++) {
				if ((await lanes.nth(i).locator('.card-title', { hasText: new RegExp(`^\\s*${t}\\s*$`) }).count()) > 0) return i;
			}
			return -1;
		};
		const card = (t: string) => titled(page, '.item-card', '.card-title', t);
		await expect(card(A())).toBeVisible();
		await expect(card(C())).toBeVisible();
		expect(await laneOf(A()), "A's lane").toBeGreaterThanOrEqual(0);
		for (let guard = 0; guard < 4 && (await laneOf(C())) !== (await laneOf(A())); guard++) {
			const dir = (await laneOf(C())) < (await laneOf(A())) ? 'Move right' : 'Move left';
			await clickMenuEntry(
				page,
				card(C()).locator('.iam-trigger'),
				page.getByRole('menuitem', { name: new RegExp(dir) }),
				async () => `C in lane ${await laneOf(C())}, A in lane ${await laneOf(A())}`
			);
			await expect(page.locator('.iam-trigger[aria-expanded="true"]')).toHaveCount(0);
		}
		expect(await laneOf(C())).toBe(await laneOf(A()));
		// Pick a card-menu move of an editable card in A's lane that gives A a
		// new index DIFFERENT from A's stored sort_order: the old renumber
		// wrote exactly those neighbours, so this is the move that used to send
		// A's refused write. (Earlier legs in this world leave A's stored value
		// wherever they left it, so the mover is not always C.)
		const lane = lanes.nth(await laneOf(A()));
		const order = (await lane.locator('.card-title').allTextContents()).map((t) => t.trim());
		const storedA = await ownerRead(`/items/${walk.grantedTask.slug}`).then((r) => (r as { sort_order: number }).sort_order);
		const moves: [RegExp, (o: string[], m: string) => string[]][] = [
			[/Move to top/, (o, m) => [m, ...o.filter((t) => t !== m)]],
			[/Move to bottom/, (o, m) => [...o.filter((t) => t !== m), m]],
			[/Move up/, (o, m) => swapWith(o, m, -1)],
			[/Move down/, (o, m) => swapWith(o, m, 1)]
		];
		const candidates = order
			.filter((t) => movable(t))
			.flatMap((mover) => moves.map(([name, f]) => ({ mover, name, next: f(order, mover) })));
		const pick = candidates.find(({ next }) => next.join() !== order.join() && next.indexOf(A()) !== storedA);
		expect(pick, `no move changes A's index away from its stored ${storedA} (lane ${order.join(', ')})`).toBeTruthy();
		// Only writes the final move sends count: the placement moves above wrote too.
		const before = writes.length;
		const moverId = idByTitle.get(pick!.mover)!;
		await clickMenuEntry(
			page,
			card(pick!.mover).locator('.iam-trigger'),
			page.getByRole('menuitem', { name: pick!.name }),
			async () => {
				const now = (await lane.locator('.card-title').allTextContents()).map((t) => t.trim());
				return `mover ${pick!.mover}; lane when picked [${order.join(', ')}], lane now [${now.join(', ')}]`;
			}
		);
		const moverLanded = () => writes.slice(before).some((w) => w.startsWith('200 ') && w.includes('sort_order') && w.includes(moverId));
		// Settle on whichever comes first: the mover's write, or a refusal (the
		// old loop stopped at A's 403, so the mover's write never came).
		await expect.poll(() => moverLanded() || refused(writes).length > 0, { timeout: 5_000 }).toBe(true);
		expect(refused(writes), 'a write the account cannot make was sent').toEqual([]);
		expect(moverLanded(), 'the move persisted').toBe(true);
	});

	test('children (door 7): drag and the Move menu on a view-only child', async ({ page }) => {
		const writes = recordWrites(page);
		await open(page, `/tasks/${walk.tasks[2].slug}`);
		await page.getByRole('tab', { name: 'Relationships' }).click();
		const child = (t: string) => titled(page, '.child-list > .child-item-wrapper', '.child-title', t);
		await expect(child(A())).toBeVisible();
		await expect(child(B()).locator('.iam-trigger'), 'control: B has the menu').toHaveCount(1);
		await expect(child(A()).locator('.iam-trigger'), 'A has no move menu').toHaveCount(0);
		expect(await pointerEngages(page, child(B()).locator('.child-row')), 'control: B drags').toBe(true);
		expect(await pointerEngages(page, child(A()).locator('.child-row')), 'A must not drag').toBe(false);
		// The control's menu still MOVES it: a write naming B lands.
		const before = writes.length;
		await clickMenuEntry(
			page,
			child(B()).locator('.iam-trigger'),
			page.getByRole('menuitem', { name: /Move (down|up|to top|to bottom)/ }).and(page.locator(':not([disabled])')),
			async () => `child ${B()}`
		);
		const bId = idByTitle.get(B())!;
		await expect.poll(() => writes.slice(before).some((w) => w.startsWith('200 ') && w.includes(bId)), { timeout: 5_000 }).toBe(true);
		expect(refused(writes)).toEqual([]);
	});

	test('children (door 7): keyboard drag (Space on the focused child)', async ({ page }) => {
		const writes = recordWrites(page);
		await open(page, `/tasks/${walk.tasks[2].slug}`);
		await page.getByRole('tab', { name: 'Relationships' }).click();
		const child = (t: string) => titled(page, '.child-list > .child-item-wrapper', '.child-title', t);
		await expect(child(A())).toBeVisible();
		expect(await keyboardEngages(page, child(A())), 'A must not lift by keyboard').toBe(false);
		const ctl = await keyboardControl(page, '.child-list', '.child-title', true);
		const before = writes.length;
		expect(await keyboardEngages(page, child(ctl.title), ctl.step), 'control: an editable child lifts and moves by keyboard').toBe(true);
		await expectMoved(writes, before);
		expect(refused(writes)).toEqual([]);
	});

	test('roles board: dropping an editable card in a lane with a view-only card writes nothing refused', async ({ page }) => {
		const writes = recordWrites(page);
		await open(page, '/roles');
		const card = (t: string) => titled(page, '.lane-items > .card-wrapper', '.card-title', t);
		// A and F share the Builder lane. Moving F past A renumbers the lane,
		// which used to name A in the batch and have the WHOLE batch refused.
		// By keyboard, so the move is deterministic: lift F, one step toward A,
		// drop.
		const lane = page.locator('.lane-items', { has: page.locator('.card-title', { hasText: new RegExp(`^\\s*${A()}\\s*$`) }) });
		// allTextContents does not wait: the board must have rendered both cards.
		await expect(card(A())).toBeVisible();
		await expect(card(F())).toBeVisible();
		const titles = (await lane.locator('.card-title').allTextContents()).map((t) => t.trim());
		const [ia, ic] = [titles.indexOf(A()), titles.indexOf(F())];
		expect(ia, 'A is in the lane').toBeGreaterThanOrEqual(0);
		expect(ic, 'F shares A\'s lane').toBeGreaterThanOrEqual(0);
		expect(await keyboardEngages(page, card(F()), ic > ia ? 'ArrowUp' : 'ArrowDown'), 'control: F lifts').toBe(true);
		const reorder = () => writes.find((w) => w.includes(' PUT ') && w.includes('/roles/board/reorder'));
		const landed = () => writes.find((w) => w.startsWith('200 PUT ') && w.includes('/roles/board/reorder'));
		await expect.poll(reorder, { timeout: 5_000 }).toBeTruthy();
		expect(reorder(), 'the batch names F').toContain(idByTitle.get(F())!);
		expect(reorder(), 'the batch must not name view-only A').not.toContain(ids.A);
		expect(landed(), 'the reorder persisted (200)').toBeTruthy();
		expect(refused(writes)).toEqual([]);
	});

	test('roles board (door 8): drag a view-only card', async ({ page }) => {
		const writes = recordWrites(page);
		await open(page, '/roles');
		const card = (t: string) => titled(page, '.lane-items > .card-wrapper', '.card-title', t);
		expect(await pointerEngages(page, card(F())), 'control: F drags').toBe(true);
		expect(await pointerEngages(page, card(A())), 'A must not drag').toBe(false);
		expect(refused(writes)).toEqual([]);
	});
});
