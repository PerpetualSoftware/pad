import { expect, type APIRequestContext, type Page } from '@playwright/test';
import { crc32, deflateSync } from 'node:zlib';
import type { SuiteFixture } from '../fixtures';

/**
 * Shared scaffolding for the attachment-viewer browser suite (TASK-2436).
 *
 * Lives here rather than in a spec because THREE specs address the same
 * surfaces (`attachment-viewer-modal`, `-owners`, `-parity`) and a
 * selector that drifts between them would silently retarget one of them.
 *
 * SELECTOR RULE, load-bearing: never a bare `[role="dialog"]`. The viewer,
 * the detail pane, `BottomSheet` and `DockedSheet` are ALL `role="dialog"`
 * now, so a bare locator matches whichever mounted last. Every locator here
 * is either class-qualified or addressed by accessible NAME.
 */

/**
 * A real, decodable 200x150 PNG — deliberately NOT the 1x1 fixture the older
 * attachment specs share.
 *
 * TWO reasons, both of which cost a debugging round to find:
 *
 *  • That fixture carries a BAD IDAT checksum. The server accepts it, but
 *    thumbnail decoding logs `decode png: invalid checksum` and skips, so the
 *    rendered `<img>` has no intrinsic size. A zero-box element is "not
 *    visible" to Playwright and cannot be clicked — which is exactly what an
 *    inline attachment in a comment body then is.
 *  • It has to be BIG. The editor's own image toolbar is absolutely positioned
 *    at the image's top-right, so a thumbnail smaller than the toolbar
 *    intercepts every pointer event aimed at the image itself.
 */
function buildPng(width: number, height: number, rgb: [number, number, number]): Buffer {
	const chunk = (type: string, data: Buffer) => {
		const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
		const len = Buffer.alloc(4);
		len.writeUInt32BE(data.length);
		const crc = Buffer.alloc(4);
		crc.writeUInt32BE(crc32(body) >>> 0);
		return Buffer.concat([len, body, crc]);
	};
	const ihdr = Buffer.alloc(13);
	ihdr.writeUInt32BE(width, 0);
	ihdr.writeUInt32BE(height, 4);
	ihdr[8] = 8; // bit depth
	ihdr[9] = 2; // colour type: truecolour
	const row = Buffer.concat([Buffer.from([0]), Buffer.from(Array.from({ length: width }, () => rgb).flat())]);
	const raw = Buffer.concat(Array.from({ length: height }, () => row));
	return Buffer.concat([
		Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
		chunk('IHDR', ihdr),
		chunk('IDAT', deflateSync(raw)),
		chunk('IEND', Buffer.alloc(0))
	]);
}

export const REAL_PNG = buildPng(200, 150, [200, 120, 60]);

export const DESKTOP = { width: 1200, height: 900 };
/** Below the 639.98px mobile breakpoint — BottomNav / DockedSheet branch. */
export const MOBILE = { width: 390, height: 844 };

const STRIP = '.attachment-strip';
export const TILE = `${STRIP} .att-tile`;
/**
 * The viewer root. Class-qualified on PURPOSE (see the selector rule above);
 * `VIEWER_ROOT_CLASS` in `$lib/a11y/viewerBackdrop` is the same string, and
 * the manager's arbitration keys on it, so a rename that broke this locator
 * would also be a real regression.
 */
export const VIEWER = '.attachment-viewer[role="dialog"]';
export const VIEWER_IMAGE = `${VIEWER} .lightbox-image`;
export const VIEWER_COUNTER = `${VIEWER} .lightbox-counter`;

/** The viewer's controls, addressed by the accessible names TASK-2429 gave them. */
export const viewerClose = (page: Page) => page.locator(VIEWER).getByRole('button', { name: 'Close' });
export const viewerNext = (page: Page) =>
	page.locator(VIEWER).getByRole('button', { name: 'Next image' });
export const viewerPrev = (page: Page) =>
	page.locator(VIEWER).getByRole('button', { name: 'Previous image' });

export function itemUrl(fixture: SuiteFixture, slug: string): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${slug}`;
}

export function collectionUrl(fixture: SuiteFixture, query = ''): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs${query}`;
}

/** Upload a file bound to `itemId` so it lands in that item's strip. */
export async function uploadAttachment(
	fixture: SuiteFixture,
	request: APIRequestContext,
	itemId: string,
	filename: string,
	mimeType = 'image/png',
	buffer: Buffer = REAL_PNG
): Promise<string> {
	const ws = fixture.workspaceSlug;
	const resp = await request.post(
		`/api/v1/workspaces/${ws}/attachments?item_id=${encodeURIComponent(itemId)}`,
		{
			headers: { Authorization: `Bearer ${fixture.apiToken}` },
			multipart: { file: { name: filename, mimeType, buffer } }
		}
	);
	if (!resp.ok()) throw new Error(`upload failed (${resp.status()}): ${await resp.text()}`);
	return ((await resp.json()) as { id: string }).id;
}

/** Post a comment on `itemSlug`; `body` is markdown. */
export async function postComment(
	fixture: SuiteFixture,
	request: APIRequestContext,
	itemSlug: string,
	body: string
): Promise<void> {
	const resp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/items/${itemSlug}/comments`,
		{
			headers: {
				Authorization: `Bearer ${fixture.apiToken}`,
				'Content-Type': 'application/json'
			},
			data: { body }
		}
	);
	if (!resp.ok()) throw new Error(`comment failed (${resp.status()}): ${await resp.text()}`);
}

/**
 * Create a throwaway collection in the suite workspace.
 *
 * Two of these tests need a list (or a `collection_updated` broadcast) that
 * NOTHING ELSE touches: the suite runs fully parallel against one server, so
 * the shared `docs` list gains rows from other specs mid-test — which is
 * exactly the interference the pre-existing `pane-a11y-focus` j/k test suffers
 * from. Isolating the collection removes the whole class.
 */
export async function createCollection(
	fixture: SuiteFixture,
	request: APIRequestContext,
	name: string
): Promise<{ slug: string }> {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authJson(fixture),
		data: { name }
	});
	if (!resp.ok()) throw new Error(`collection create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { slug: string };
}

/**
 * Best-effort cleanup. It WARNS rather than throws on failure, deliberately:
 * every caller runs it from a `finally`, and an exception thrown there would
 * REPLACE the test's real failure with a cleanup error. A leaked scratch
 * collection is visible in the warning and harmless to later specs; a
 * swallowed assertion failure is neither.
 */
export async function deleteCollection(
	fixture: SuiteFixture,
	request: APIRequestContext,
	slug: string
): Promise<void> {
	try {
		const resp = await request.delete(
			`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${slug}`,
			{ headers: { Authorization: `Bearer ${fixture.apiToken}` } }
		);
		if (!resp.ok()) {
			console.warn(`[attachment-viewer] leaked collection ${slug}: delete returned ${resp.status()}`);
		}
	} catch (err) {
		// A REJECTED request (the context is torn down after a timeout, say) would
		// propagate out of the caller's `finally` and replace the real failure,
		// which is the whole thing this helper exists to avoid.
		console.warn(`[attachment-viewer] leaked collection ${slug}: ${String(err)}`);
	}
}

/** Same contract as {@link deleteCollection}, for a scratch workspace. */
export async function deleteWorkspace(
	fixture: SuiteFixture,
	request: APIRequestContext,
	slug: string
): Promise<void> {
	try {
		const resp = await request.delete(`/api/v1/workspaces/${slug}`, {
			headers: { Authorization: `Bearer ${fixture.apiToken}` }
		});
		if (!resp.ok()) {
			console.warn(`[attachment-viewer] leaked workspace ${slug}: delete returned ${resp.status()}`);
		}
	} catch (err) {
		console.warn(`[attachment-viewer] leaked workspace ${slug}: ${String(err)}`);
	}
}

/** Create an item in an arbitrary collection of the suite workspace. */
export async function createItem(
	fixture: SuiteFixture,
	request: APIRequestContext,
	collectionSlug: string,
	title: string
): Promise<{ id: string; slug: string; ref: string }> {
	const resp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collectionSlug}/items`,
		{ headers: authJson(fixture), data: { title, fields: '{}', content: '' } }
	);
	if (!resp.ok()) throw new Error(`item create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { id: string; slug: string; ref: string };
}

export function authJson(fixture: SuiteFixture): Record<string, string> {
	return {
		Authorization: `Bearer ${fixture.apiToken}`,
		'Content-Type': 'application/json'
	};
}

/**
 * Drop a file onto the live editor, the way a user does — the upload plugin
 * listens for a real `drop` with a DataTransfer. Same shape as
 * `item-attachment-strip.spec.ts`, which predates this module.
 */
export async function dropFileIntoEditor(
	page: Page,
	filename: string,
	base64: string,
	mimeType = 'image/png',
	editorSelector = '.editor-content .ProseMirror'
): Promise<void> {
	const target = page.locator(editorSelector).first();
	await target.waitFor({ state: 'visible' });
	await target.evaluate(
		(el, { filename, base64, mimeType }) => {
			const bytes = Uint8Array.from(atob(base64), (c) => c.charCodeAt(0));
			const file = new File([bytes], filename, { type: mimeType });
			const dt = new DataTransfer();
			dt.items.add(file);
			const rect = el.getBoundingClientRect();
			el.dispatchEvent(
				new DragEvent('drop', {
					bubbles: true,
					cancelable: true,
					dataTransfer: dt,
					clientX: rect.left + 8,
					clientY: rect.top + 8
				})
			);
		},
		{ filename, base64, mimeType }
	);
}

/**
 * Can `selector` actually TAKE focus right now?
 *
 * This is the inertness oracle for the whole suite: an inert element (or one
 * inside an inert subtree) refuses `focus()` silently, and jsdom's polyfill
 * cannot reproduce that — it only toggles the attribute. Asserting the
 * ATTRIBUTE would pass against a manager that wrote `inert` on the wrong
 * elements; asserting focusability cannot.
 */
export function isFocusable(page: Page, selector: string): Promise<boolean> {
	return page.evaluate((sel) => {
		const el = document.querySelector<HTMLElement>(sel);
		if (!el) throw new Error(`isFocusable: no element for ${sel}`);
		const before = document.activeElement;
		el.focus();
		const took = document.activeElement === el;
		if (!took && before instanceof HTMLElement) before.focus();
		return took;
	}, selector);
}

/** Is focus inside the frontmost `.attachment-viewer`? */
export function activeInViewer(page: Page): Promise<boolean> {
	return page.evaluate(() => {
		const viewers = Array.from(document.querySelectorAll('.attachment-viewer'));
		const front = viewers[viewers.length - 1];
		return !!front && !!document.activeElement && front.contains(document.activeElement);
	});
}

/**
 * Append a focusable PROBE button to every `<body>` child and report which of
 * them can take focus right now.
 *
 * Why probes at all: the app's own body children are mostly portal roots that
 * are empty (or `display: none`) between overlays, so "nothing in them is
 * focusable" would be true with or without a backdrop. A probe makes each
 * child's inertness observable INDEPENDENTLY of what the app happens to have
 * rendered there.
 *
 * Returns one entry per body child in document order. `viewer` marks the
 * viewer's own root so a caller can assert the exemption.
 */
interface BodyChildProbe {
	index: number;
	viewer: boolean;
	inert: boolean;
	probeFocusable: boolean;
	/** false for the `display: none` portal roots, where focusability proves nothing. */
	probeMeaningful: boolean;
}

function probeBodyChildren(page: Page): Promise<BodyChildProbe[]> {
	return page.evaluate(() => {
		const results: BodyChildProbe[] = [];
		Array.from(document.body.children).forEach((child, index) => {
			let probe = child.querySelector<HTMLButtonElement>(':scope > [data-viewer-probe]');
			if (!probe) {
				probe = document.createElement('button');
				probe.dataset.viewerProbe = String(index);
				probe.textContent = 'probe';
				// Fixed + on-screen so nothing about layout (a zero-size or
				// off-screen parent) is what makes it unfocusable.
				probe.style.cssText = 'position:fixed;top:0;left:0;opacity:0;';
				child.appendChild(probe);
			}
			const before = document.activeElement;
			probe.focus();
			const probeFocusable = document.activeElement === probe;
			if (!probeFocusable && before instanceof HTMLElement) before.focus();
			results.push({
				index,
				viewer: child.classList.contains('attachment-viewer'),
				inert: child.hasAttribute('inert'),
				probeFocusable,
				// A probe inside a `display: none` root is unfocusable for reasons
				// that have nothing to do with the backdrop; the caller must not
				// count it as evidence either way.
				probeMeaningful: getComputedStyle(child).display !== 'none'
			});
		});
		return results;
	}) as Promise<BodyChildProbe[]>;
}

/**
 * Every body child except the viewer must be inert, and every MEANINGFUL probe
 * outside the viewer must have lost focusability — while the viewer's own
 * probe keeps it.
 *
 * `minMeaningful` guards against the whole assertion going vacuous if the app
 * shell ever stops rendering the children this depends on. ONE is the floor,
 * not the observed count: `<body>` has four children on these pages (the
 * SvelteKit `display: contents` wrapper, an offscreen portal root, and two
 * `display: none` portal roots), but only the wrapper is guaranteed — and it
 * is the one whose inertness the app depends on. Requiring more would turn an
 * unrelated portal change into a failure of this assertion.
 */
export async function expectBackgroundInert(page: Page, minMeaningful = 1): Promise<void> {
	const probes = await probeBodyChildren(page);
	const viewers = probes.filter((p) => p.viewer);
	expect(viewers.length, 'a viewer body child must exist').toBeGreaterThan(0);
	const background = probes.filter((p) => !p.viewer);
	const meaningful = background.filter((p) => p.probeMeaningful);
	expect(
		meaningful.length,
		'the focusability half of this assertion needs at least one laid-out background body child'
	).toBeGreaterThanOrEqual(minMeaningful);
	for (const p of background) {
		expect(p.inert, `body child ${p.index} must be inert while a viewer is open`).toBe(true);
		if (p.probeMeaningful) {
			expect(
				p.probeFocusable,
				`an injected button in body child ${p.index} must be unreachable`
			).toBe(false);
		}
	}
	// The FRONTMOST viewer stays interactive. A background viewer (two stacked)
	// is inert like everything else, which is asserted where that case is set up.
	const front = viewers[viewers.length - 1];
	expect(front.inert, 'the frontmost viewer must not be inert').toBe(false);
	expect(front.probeFocusable, 'the frontmost viewer must stay reachable').toBe(true);
}

/**
 * Try to move focus to `selector` and report whether it took — WITHOUT leaving
 * focus there if it did. The counterpart to {@link isFocusable} for "prove the
 * background really is unreachable" assertions inside a Tab-trap test, where
 * observing only that Tab stayed put would also pass against a JS trap running
 * over a background that is not actually inert.
 */
export async function focusAttemptTakes(page: Page, selector: string): Promise<boolean> {
	return page.evaluate((sel) => {
		const el = document.querySelector<HTMLElement>(sel);
		if (!el) throw new Error(`focusAttemptTakes: no element for ${sel}`);
		const before = document.activeElement;
		el.focus();
		const took = document.activeElement === el;
		if (before instanceof HTMLElement) before.focus();
		return took;
	}, selector);
}

/** Remove every injected probe so a later assertion can't trip over them. */
export function clearProbes(page: Page): Promise<void> {
	return page.evaluate(() => {
		document.querySelectorAll('[data-viewer-probe]').forEach((p) => p.remove());
	});
}
