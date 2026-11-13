// Preview harness for the MCP Apps item card. NOT part of the build and never
// embedded: it writes a stand-alone host page that loads the built card
// (web/build-mcp-app/item-card.html) in a sandboxed iframe and plays the host
// side of the protocol with the SDK's own AppBridge and a canned server.
//
//   node scripts/build-mcp-app.mjs
//   node scripts/preview-mcp-app.mjs <out-dir>           # writes <out-dir>/preview.html
//   node scripts/preview-mcp-app.mjs <out-dir> --shots   # also screenshots every state
//
// Open preview.html?theme=dark&state=conflict&vars=1 by hand to poke at it.
// States: normal, saving, conflict, error, empty, stub.
import { build } from 'esbuild';
import path from 'node:path';
import fs from 'node:fs';
import { fileURLToPath, pathToFileURL } from 'node:url';

const web = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const [outArg, ...flags] = process.argv.slice(2);
if (!outArg || outArg.startsWith('--')) {
	console.error('usage: node scripts/preview-mcp-app.mjs <out-dir> [--shots]');
	process.exit(2);
}
const outDir = path.resolve(outArg);
const cardPath = path.join(web, 'build-mcp-app/item-card.html');
if (!fs.existsSync(cardPath) || fs.statSync(cardPath).size < 1024) {
	console.error('preview-mcp-app: build the card first (node scripts/build-mcp-app.mjs)');
	process.exit(1);
}

// Runs in the preview page. `CARD_HTML` is defined at bundle time.
const hostSource = String.raw`
import { AppBridge, PostMessageTransport } from '@modelcontextprotocol/ext-apps/app-bridge';

const params = new URLSearchParams(location.search);
const theme = params.get('theme') === 'dark' ? 'dark' : 'light';
const state = params.get('state') ?? 'normal';
document.documentElement.dataset.theme = theme;

// A deliberately un-Pad palette, to show the card takes the host's variables.
const hostVariables = theme === 'dark'
	? { '--color-background-primary': '#14202b', '--color-background-secondary': '#1d2e3d', '--color-text-primary': '#e6f0f7', '--color-text-secondary': '#93a9ba', '--color-border-primary': '#35506a', '--color-ring-primary': '#5cc8ff', '--font-sans': 'Georgia, serif', '--border-radius-md': '2px' }
	: { '--color-background-primary': '#fdf8ee', '--color-background-secondary': '#f1e7d0', '--color-text-primary': '#3a2c12', '--color-text-secondary': '#7d6a45', '--color-border-primary': '#d5c29a', '--color-ring-primary': '#b3590a', '--font-sans': 'Georgia, serif', '--border-radius-md': '2px' };

const schema = { fields: [
	{ key: 'status', label: 'Status', type: 'select', options: ['open', 'in-progress', 'in-review', 'done', 'cancelled'] },
	{ key: 'priority', label: 'Priority', type: 'select', options: ['low', 'medium', 'high', 'critical'] },
	{ key: 'effort', label: 'Effort', type: 'select', options: ['xs', 's', 'm', 'l', 'xl'] },
	{ key: 'health', label: 'Health', type: 'select', options: ['on-track', 'at-risk'], computed: true },
	{ key: 'due', label: 'Due', type: 'date' }
] };
let item = {
	id: 'a1', ref: 'TASK-128', seq: 41, title: 'Redirect back to the requested page after sign-in',
	collection_slug: 'tasks', collection_name: 'Tasks', updated_at: '2026-11-12T16:42:00Z',
	assigned_user_name: 'Ada Lovelace', content_state: params.get('stale') ? 'applied_pending_flush' : undefined,
	fields: { status: 'in-progress', priority: 'high', effort: 'm', health: 'on-track' },
	content: '## Problem\n\nSigning in from a deep link drops the user on the dashboard instead of the page they asked for. <img src=x onerror=alert(1)>\n\n## Plan\n\n- keep the requested path in the sign-in state parameter\n- validate it against same-origin paths before redirecting\n- fall back to the dashboard when it is missing or unsafe\n\nSee TASK-97 for the earlier attempt and why the cookie approach was dropped.'
};
const ok = body => ({ structuredContent: body, content: [{ type: 'text', text: JSON.stringify(body) }] });
const fail = (code, message) => {
	const body = { error: { code, message } };
	return { isError: true, structuredContent: body, content: [{ type: 'text', text: JSON.stringify(body) }] };
};
const calls = [];
window.previewCalls = calls;

async function callTool({ name, arguments: args = {} }) {
	calls.push({ name, args });
	if (name === 'pad_collection') return ok({ items: [{ slug: 'tasks', name: 'Tasks', schema: JSON.stringify(schema) }] });
	if (name !== 'pad_item') return fail('not_found', 'unknown tool');
	if (args.action === 'get') return state === 'stub' ? fail('not_found', 'Item not found') : ok(item);
	if ('content' in args) return fail('validation_failed', 'the card must never send content');
	if (state === 'saving') return new Promise(() => {});
	if (state === 'conflict') {
		item = { ...item, seq: item.seq + 2, title: 'Return to the requested page after sign-in', fields: { ...item.fields, status: 'in-review' } };
		return fail('update_conflict', 'item changed since seq ' + args.expected_seq);
	}
	if (state === 'error') return fail('forbidden', 'You have view-only access to this workspace');
	const fields = { ...item.fields };
	for (const pair of args.field ?? []) fields[pair.slice(0, pair.indexOf('='))] = pair.slice(pair.indexOf('=') + 1);
	item = { ...item, seq: item.seq + 1, title: args.title ?? item.title, fields, updated_at: '2026-11-13T09:05:00Z' };
	await new Promise(resolve => setTimeout(resolve, 400));
	return ok(item);
}

const frame = document.querySelector('iframe');
const bridge = new AppBridge(null, { name: 'card-preview', version: '0' }, { serverTools: {} }, {
	hostContext: {
		theme, displayMode: 'inline', locale: 'en-GB', timeZone: 'UTC', platform: 'web',
		...(params.get('vars') ? { styles: { variables: hostVariables } } : {})
	}
});
bridge.oncalltool = callTool;
bridge.onsizechange = ({ height }) => {
	if (typeof height !== 'number') return;
	frame.style.height = height + 'px';
	document.body.dataset.cardHeight = String(height);
};
bridge.oninitialized = async () => {
	if (state === 'empty') {
		await bridge.sendToolInput({ arguments: { action: 'list', collection: 'tasks' } });
		await bridge.sendToolResult(ok({ items: [item] }));
	} else if (state === 'stub') {
		await bridge.sendToolInput({ arguments: { action: 'get', ref: 'TASK-128' } });
		await bridge.sendToolResult({ content: [{ type: 'text', text: 'ok' }] });
	} else {
		await bridge.sendToolInput({ arguments: { action: 'get', ref: 'TASK-128' } });
		await bridge.sendToolResult(ok(item));
	}
	document.body.dataset.ready = '1';
};
// Listen before the card loads: it starts the handshake as soon as its script runs.
await bridge.connect(new PostMessageTransport(frame.contentWindow, frame.contentWindow));
frame.srcdoc = CARD_HTML;
`;

const bundled = await build({
	stdin: { contents: hostSource, resolveDir: web, loader: 'js' },
	bundle: true, format: 'esm', platform: 'browser', target: 'es2022', write: false, logLevel: 'warning',
	define: { CARD_HTML: JSON.stringify(fs.readFileSync(cardPath, 'utf8')) }
});
const hostScript = bundled.outputFiles[0].text.replace(/<\/script/gi, '<\\/script');

const page = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Pad item card preview</title>
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
</style>
</head>
<body>
<main>
	<p class="user">What is the state of the sign-in redirect task?</p>
	<p class="tool">Pad &middot; pad_item</p>
	<div class="frame"><iframe title="Pad item card" sandbox="allow-scripts"></iframe></div>
	<p>It is in progress and assigned to Ada, at high priority. The plan is to carry the requested path through the sign-in state parameter.</p>
</main>
<script type="module">${hostScript}</script>
</body>
</html>
`;
fs.mkdirSync(outDir, { recursive: true });
const previewPath = path.join(outDir, 'preview.html');
fs.writeFileSync(previewPath, page);
console.log(`preview-mcp-app: wrote ${previewPath}`);

if (flags.includes('--shots')) {
	const { chromium } = await import('@playwright/test');
	const browser = await chromium.launch();
	const failures = [];
	try {
		for (const [width, height] of [[1280, 720], [390, 760], [320, 760]]) {
			for (const theme of ['light', 'dark']) {
				const shots = width === 320
					? [['normal', '']]
					: [['normal', ''], ['saving', ''], ['conflict', ''], ['error', ''], ['empty', ''], ['stub', ''], ['title-edit', ''], ['normal', 'vars'], ['normal', 'stale']];
				for (const [shot, extra] of shots) {
					const state = shot === 'title-edit' ? 'normal' : shot;
					const context = await browser.newContext({ viewport: { width, height }, colorScheme: theme });
					const tab = await context.newPage();
					const errors = [];
					tab.on('pageerror', error => errors.push(String(error)));
					tab.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
					const url = new URL(pathToFileURL(previewPath));
					url.search = new URLSearchParams({ theme, state, ...(extra ? { [extra]: '1' } : {}) }).toString();
					await tab.goto(url.href);
					await tab.waitForSelector('body[data-ready]');
					const card = tab.frameLocator('iframe');
					if (state === 'empty') {
						await tab.waitForTimeout(300);
						const cardHeight = await tab.evaluate(() => document.querySelector('iframe').getBoundingClientRect().height);
						if (cardHeight !== 0) failures.push(`${shot} ${theme} ${width}: empty card is ${cardHeight}px tall`);
					} else if (state === 'stub') {
						await card.getByText('TASK-128').waitFor();
					} else {
						await card.getByLabel('Status').waitFor();
						if (state !== 'normal') await card.getByLabel('Status').selectOption('done');
						if (state === 'saving') await card.getByText('Saving…').waitFor();
						if (state === 'conflict') await card.getByText('Changed elsewhere; reloaded').waitFor();
						if (state === 'error') await card.getByRole('alert').waitFor();
						if (shot === 'title-edit') await card.getByRole('button', { name: 'Edit title' }).click();
						if (await card.locator('img').count() > 0) failures.push(`${shot}: content rendered an img element`);
						const overflow = await card.locator('html').evaluate(el => el.scrollWidth - el.clientWidth);
						if (overflow > 0) failures.push(`${shot} ${theme} ${width}: card overflows horizontally by ${overflow}px`);
					}
					await tab.waitForTimeout(250);
					if (state !== 'empty' && state !== 'normal' && state !== 'stub') {
						const sent = await tab.evaluate(() => window.previewCalls.filter(call => call.args.action === 'update'));
						if (sent.length !== 1 || 'content' in sent[0].args || typeof sent[0].args.expected_seq !== 'number') failures.push(`${shot}: unexpected update calls ${JSON.stringify(sent)}`);
					}
					if (errors.length > 0) failures.push(`${shot} ${theme} ${width}: ${errors.join(' | ')}`);
					const name = `${shot}${extra ? '-' + extra : ''}-${theme}-${width}.png`;
					await tab.screenshot({ path: path.join(outDir, name), fullPage: true });
					console.log(`preview-mcp-app: ${name}`);
					await context.close();
				}
			}
		}
	} finally {
		await browser.close();
	}
	if (failures.length > 0) {
		console.error('preview-mcp-app: FAILED\n  ' + failures.join('\n  '));
		process.exit(1);
	}
}
