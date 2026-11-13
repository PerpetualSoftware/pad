// Preview harness for the MCP Apps board. NOT part of the build and never
// embedded: the board twin of preview-mcp-app.mjs. It writes a stand-alone host
// page that loads the built board (web/build-mcp-app/board.html) in a sandboxed
// iframe and plays the host side with the SDK's own AppBridge and a canned server.
//
//   node scripts/build-mcp-app.mjs
//   node scripts/preview-mcp-board.mjs <out-dir>           # writes <out-dir>/board-preview.html
//   node scripts/preview-mcp-board.mjs <out-dir> --shots   # also screenshots the states
//
// Open board-preview.html?mode=inline&theme=dark&state=conflict&vars=1 by hand to poke at it.
// Modes: fullscreen (default), inline. States: normal, saving, conflict, error.
import { build } from 'esbuild';
import path from 'node:path';
import fs from 'node:fs';
import { fileURLToPath, pathToFileURL } from 'node:url';

const web = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const [outArg, ...flags] = process.argv.slice(2);
if (!outArg || outArg.startsWith('--')) {
	console.error('usage: node scripts/preview-mcp-board.mjs <out-dir> [--shots]');
	process.exit(2);
}
const outDir = path.resolve(outArg);
const boardPath = path.join(web, 'build-mcp-app/board.html');
if (!fs.existsSync(boardPath) || fs.statSync(boardPath).size < 1024) {
	console.error('preview-mcp-board: build the board first (node scripts/build-mcp-app.mjs)');
	process.exit(1);
}

// Runs in the preview page. `BOARD_HTML` is defined at bundle time.
const hostSource = String.raw`
import { AppBridge, PostMessageTransport } from '@modelcontextprotocol/ext-apps/app-bridge';

const params = new URLSearchParams(location.search);
const theme = params.get('theme') === 'dark' ? 'dark' : 'light';
const mode = params.get('mode') === 'inline' ? 'inline' : 'fullscreen';
const state = params.get('state') ?? 'normal';
document.documentElement.dataset.theme = theme;
document.documentElement.dataset.mode = mode;

// A deliberately un-Pad palette, to show the board takes the host's variables.
const hostVariables = theme === 'dark'
	? { '--color-background-primary': '#14202b', '--color-background-secondary': '#1d2e3d', '--color-text-primary': '#e6f0f7', '--color-text-secondary': '#93a9ba', '--color-border-primary': '#35506a', '--color-ring-primary': '#5cc8ff', '--font-sans': 'Georgia, serif', '--border-radius-md': '2px' }
	: { '--color-background-primary': '#fdf8ee', '--color-background-secondary': '#f1e7d0', '--color-text-primary': '#3a2c12', '--color-text-secondary': '#7d6a45', '--color-border-primary': '#d5c29a', '--color-ring-primary': '#b3590a', '--font-sans': 'Georgia, serif', '--border-radius-md': '2px' };

let n = 0;
const row = (ref, title, status, priority = '', assignee = '') => ({
	id: 'id-' + ref, ref, title, status, priority, assignee, parent_ref: 'PLAN-12', seq: 40 + n,
	updated_at: new Date(Date.UTC(2026, 10, 12, 16, 0) - 3600_000 * n++).toISOString()
});
const columns = [
	{ status: 'open', terminal: false, abandoned: false, items: [
		row('TASK-131', 'Rate-limit the password reset endpoint per account', 'open', 'high', 'Ada Lovelace'),
		row('TASK-134', 'Show the last sign-in time on the security page <img src=x onerror=alert(1)>', 'open', 'medium'),
		row('TASK-137', 'Session list: name the device and browser', 'open', 'low', 'Grace Hopper'),
		row('BUG-58', 'Sign-out leaves the refresh cookie behind on Safari', 'open', 'critical'),
		row('TASK-140', 'Document the recovery-code flow', 'open')
	] },
	{ status: 'in-progress', terminal: false, abandoned: false, items: [
		row('TASK-128', 'Redirect back to the requested page after sign-in', 'in-progress', 'high', 'Ada Lovelace'),
		row('TASK-126', 'Passkey enrolment from the account menu', 'in-progress', 'medium', 'Linus Okafor'),
		row('BUG-55', 'Two-factor prompt loops when the clock is skewed by more than a minute', 'in-progress', 'high', 'Grace Hopper')
	] },
	{ status: 'review', terminal: false, abandoned: false, items: [
		row('TASK-121', 'Lock an account after ten failed attempts', 'review', 'high', 'Linus Okafor'),
		row('TASK-119', 'Email a notice when a new device signs in', 'review', 'medium', 'Ada Lovelace')
	] },
	{ status: 'done', terminal: true, abandoned: false, total: 57, items: [
		row('TASK-117', 'Rotate session tokens on privilege change', 'done', 'high', 'Grace Hopper'),
		row('TASK-112', 'Hash recovery codes at rest', 'done', 'high', 'Ada Lovelace'),
		row('BUG-51', 'Sign-in form loses the email on a failed attempt', 'done', 'low', 'Linus Okafor'),
		row('TASK-108', 'Add the sign-in audit log', 'done', 'medium', 'Grace Hopper')
	] },
	{ status: 'cancelled', terminal: true, abandoned: true, items: [
		row('TASK-97', 'Carry the return path in a cookie', 'cancelled', 'medium', 'Ada Lovelace'),
		row('TASK-90', 'SMS one-time codes', 'cancelled', 'low')
	] }
].map(column => ({ total: column.items.length, ...column }));
const snapshot = { workspace: 'acme', collection: 'tasks', columns, truncated: true, unplaced: 1, cursor: 'c1', access_epoch: 'e1' };

const ok = body => ({ structuredContent: body, content: [{ type: 'text', text: JSON.stringify(body) }] });
const fail = (code, message) => {
	const body = { error: { code, message } };
	return { isError: true, structuredContent: body, content: [{ type: 'text', text: JSON.stringify(body) }] };
};
const calls = [];
window.previewCalls = calls;
const find = ref => columns.flatMap(column => column.items).find(item => item.ref === ref);
// What pad_item answers with: the full item, body included, status inside fields.
const full = (item, over = {}) => ({
	id: item.id, ref: item.ref, title: item.title, seq: item.seq, collection_slug: 'tasks', parent_ref: item.parent_ref,
	assigned_user_name: item.assignee || undefined, updated_at: '2026-11-13T09:05:00Z',
	content: 'ITEM BODY: the board must never show this.', fields: { status: item.status, priority: item.priority }, ...over
});

async function callTool({ name, arguments: args = {} }) {
	calls.push({ name, args });
	if (name === 'pad_board') {
		return args.action === 'show' ? ok(snapshot) : ok({ upserts: [], removed_ids: [], cursor: 'c1', access_epoch: 'e1', resync: false });
	}
	if (name !== 'pad_item') return fail('not_found', 'unknown tool');
	const item = find(args.ref);
	if (!item) return fail('item_not_found', 'Item not found');
	// The reload after a conflict: someone else moved it to review and renamed it.
	if (args.action === 'get') return ok(full(item, { seq: item.seq + 2, title: 'Rate-limit password resets per account and per address', fields: { status: 'review', priority: 'high' } }));
	if ('content' in args || 'title' in args) return fail('validation_failed', 'the board must only ever send a status');
	if (state === 'saving') return new Promise(() => {});
	if (state === 'conflict') return fail('update_conflict', 'item changed since seq ' + args.expected_seq);
	if (state === 'error') return fail('permission_denied', 'You have view-only access to this workspace');
	const status = args.field[0].slice('status='.length);
	await new Promise(resolve => setTimeout(resolve, 400));
	return ok(full(item, { seq: item.seq + 1, fields: { status, priority: item.priority } }));
}

const frame = document.querySelector('iframe');
const bridge = new AppBridge(null, { name: 'board-preview', version: '0' }, { serverTools: {} }, {
	hostContext: {
		theme, displayMode: mode, availableDisplayModes: ['inline', 'fullscreen'], locale: 'en-GB', timeZone: 'UTC', platform: 'web',
		...(params.get('vars') ? { styles: { variables: hostVariables } } : {})
	}
});
bridge.oncalltool = callTool;
bridge.onsizechange = ({ height }) => {
	if (mode !== 'inline' || typeof height !== 'number') return;
	frame.style.height = height + 'px';
};
bridge.oninitialized = async () => {
	await bridge.sendToolInput({ arguments: { action: 'show', workspace: 'acme', collection: 'tasks', parent: 'PLAN-12' } });
	await bridge.sendToolResult(ok(snapshot));
	document.body.dataset.ready = '1';
};
// Listen before the board loads: it starts the handshake as soon as its script runs.
await bridge.connect(new PostMessageTransport(frame.contentWindow, frame.contentWindow));
frame.srcdoc = BOARD_HTML;
`;

const bundled = await build({
	stdin: { contents: hostSource, resolveDir: web, loader: 'js' },
	bundle: true, format: 'esm', platform: 'browser', target: 'es2022', write: false, logLevel: 'warning',
	define: { BOARD_HTML: JSON.stringify(fs.readFileSync(boardPath, 'utf8')) }
});
const hostScript = bundled.outputFiles[0].text.replace(/<\/script/gi, '<\\/script');

const page = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Pad board preview</title>
<style>
	:root { color-scheme: light; --page: #faf9f5; --ink: #1f1e1c; --dim: #6f6d66; --line: #dedbd2; --bubble: #f0eee6; }
	:root[data-theme='dark'] { color-scheme: dark; --page: #1b1b19; --ink: #f2f0ea; --dim: #a5a299; --line: #45443f; --bubble: #2c2c2a; }
	body { margin: 0; background: var(--page); color: var(--ink); font: 15px/1.5 system-ui, sans-serif; }
	main { max-width: 760px; margin: 0 auto; padding: 20px 16px 28px; display: flex; flex-direction: column; gap: 14px; }
	.user { align-self: flex-end; max-width: 80%; padding: 8px 14px; border-radius: 14px; background: var(--bubble); }
	.tool { color: var(--dim); font-size: 13px; }
	/* What a host does for a view that asks for prefersBorder. */
	.frame { border: 1px solid var(--line); border-radius: 12px; overflow: hidden; }
	iframe { display: block; width: 100%; height: 0; border: 0; }
	p { margin: 0; }
	/* Fullscreen: the view owns the whole viewport and the conversation is gone. */
	:root[data-mode='fullscreen'] main > :not(.frame) { display: none; }
	:root[data-mode='fullscreen'] main { max-width: none; padding: 0; }
	:root[data-mode='fullscreen'] .frame { border: 0; border-radius: 0; }
	:root[data-mode='fullscreen'] iframe { height: 100vh; }
</style>
</head>
<body>
<main>
	<p class="user">Show me the board for the sign-in hardening plan.</p>
	<p class="tool">Pad &middot; pad_board</p>
	<div class="frame"><iframe title="Pad board" sandbox="allow-scripts"></iframe></div>
	<p>Five tasks are open and three are in progress. Two are waiting for review.</p>
</main>
<script type="module">${hostScript}</script>
</body>
</html>
`;
fs.mkdirSync(outDir, { recursive: true });
const previewPath = path.join(outDir, 'board-preview.html');
fs.writeFileSync(previewPath, page);
console.log(`preview-mcp-board: wrote ${previewPath}`);

if (flags.includes('--shots')) {
	const { chromium } = await import('@playwright/test');
	const browser = await chromium.launch();
	const failures = [];
	// [name, width, height, theme, mode, state, extra]
	const shots = [];
	for (const [size, width, height] of [['desktop', 1280, 800], ['phone', 390, 780]]) {
		for (const theme of ['light', 'dark']) shots.push([`fullscreen-${size}-${theme}`, width, height, theme, 'fullscreen', 'normal', '']);
		shots.push([`inline-${size}-light`, width, height, 'light', 'inline', 'normal', '']);
		shots.push([`conflict-${size}-light`, width, height, 'light', 'fullscreen', 'conflict', '']);
	}
	shots.push(['conflict-desktop-dark', 1280, 800, 'dark', 'fullscreen', 'conflict', '']);
	shots.push(['saving-desktop-light', 1280, 800, 'light', 'fullscreen', 'saving', '']);
	shots.push(['error-desktop-light', 1280, 800, 'light', 'fullscreen', 'error', '']);
	shots.push(['fullscreen-desktop-light-vars', 1280, 800, 'light', 'fullscreen', 'normal', 'vars']);
	shots.push(['fullscreen-desktop-dark-vars', 1280, 800, 'dark', 'fullscreen', 'normal', 'vars']);
	try {
		for (const [name, width, height, theme, mode, state, extra] of shots) {
			const context = await browser.newContext({ viewport: { width, height }, colorScheme: theme, hasTouch: width < 600, isMobile: width < 600 });
			const tab = await context.newPage();
			const errors = [];
			tab.on('pageerror', error => errors.push(String(error)));
			tab.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
			const url = new URL(pathToFileURL(previewPath));
			url.search = new URLSearchParams({ theme, mode, state, ...(extra ? { [extra]: '1' } : {}) }).toString();
			await tab.goto(url.href);
			await tab.waitForSelector('body[data-ready]');
			const board = tab.frameLocator('iframe');
			const control = board.getByLabel('Status of TASK-131');
			await control.waitFor();
			if (state !== 'normal') await control.selectOption('done');
			if (state === 'saving') await board.getByText('Saving…').waitFor();
			if (state === 'conflict') await board.getByText('Changed elsewhere; reloaded').waitFor();
			if (state === 'error') await board.getByRole('alert').waitFor();
			await tab.waitForTimeout(250);
			if (await board.locator('img').count() > 0) failures.push(`${name}: a title rendered an img element`);
			if (await board.getByText('ITEM BODY').count() > 0) failures.push(`${name}: item content is on screen`);
			if (mode === 'fullscreen') {
				// The page itself must not scroll: only the column strip and the columns do.
				const overflow = await board.locator('html').evaluate(el => [el.scrollWidth - el.clientWidth, el.scrollHeight - el.clientHeight]);
				if (overflow[0] > 0 || overflow[1] > 0) failures.push(`${name}: the view overflows its frame by ${overflow.join('x')}px`);
			}
			const sent = await tab.evaluate(() => window.previewCalls);
			const updates = sent.filter(call => call.args.action === 'update');
			if (updates.length !== (state === 'normal' ? 0 : 1)) failures.push(`${name}: ${updates.length} update calls`);
			for (const update of updates) {
				if (Object.keys(update.args).sort().join() !== 'action,expected_seq,field,ref,workspace' || update.args.field.join() !== 'status=done') failures.push(`${name}: unexpected update ${JSON.stringify(update.args)}`);
			}
			const allowed = ['pad_board:show', 'pad_board:changes', 'pad_item:get', 'pad_item:update'];
			for (const call of sent) if (!allowed.includes(`${call.name}:${call.args.action}`)) failures.push(`${name}: unexpected call ${call.name}:${call.args.action}`);
			if (errors.length > 0) failures.push(`${name}: ${errors.join(' | ')}`);
			await tab.screenshot({ path: path.join(outDir, `${name}.png`), fullPage: mode === 'inline' });
			console.log(`preview-mcp-board: ${name}.png`);
			await context.close();
		}
	} finally {
		await browser.close();
	}
	if (failures.length > 0) {
		console.error('preview-mcp-board: FAILED\n  ' + failures.join('\n  '));
		process.exit(1);
	}
}
