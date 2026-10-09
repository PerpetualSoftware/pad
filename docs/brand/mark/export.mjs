#!/usr/bin/env node
// Exports the Pad mark's web icon set from the master SVGs (TASK-3514).
//
//   node docs/brand/mark/export.mjs web/static            # this repo's app
//   node docs/brand/mark/export.mjs ../pad-web/static     # getpad.dev
//
// Writes, into the given directory:
//   favicon-16x16.png, favicon-32x32.png  pad-mark-small, at 1x and 2x
//   favicon.ico                           the same at 16, 32 and 48 (PNG-in-ICO)
//   icon.svg                              pad-mark-small (the SVG favicon a browser
//                                         prefers in a tab, so it must read at 16px)
//   pad-mark.svg                          the full mark, for headers and lockups
//   icon-192.png, icon-512.png            pad-app-icon-shaped (PWA / manifest)
//   apple-touch-icon.png                  pad-app-icon at 180, full-bleed (iOS masks it)
//   padicon.png                           pad-app-icon-shaped at 512 (kept for links
//                                         to the old file name)
//   og-card.png                           with --og: the 1200x630 link-preview card
//                                         (the app's og:image; no URL on it, since a
//                                         self-hosted server serves the same file)
//
// Every PNG is a render of a master SVG, never an edit of a PNG. Needs
// web/node_modules (`npm ci` in web/) for Playwright.

import { copyFileSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(HERE, '..', '..', '..');
const require = createRequire(join(ROOT, 'web', 'package.json'));
const { chromium } = require('@playwright/test');

const args = process.argv.slice(2);
const withOG = args.includes('--og');
const out = args.find((a) => !a.startsWith('--'));
if (!out) {
	console.error('usage: node docs/brand/mark/export.mjs <static-dir> [--og]');
	process.exit(2);
}
const OUT = resolve(out);
const src = (name) => join(HERE, name);

// A PNG of svg at size x size, transparent where the SVG is.
async function render(page, svgFile, size) {
	const dir = mkdtempSync(join(tmpdir(), 'pad-mark-'));
	const html = join(dir, 'r.html');
	writeFileSync(html, `<!doctype html><style>html,body{margin:0;background:transparent}img{display:block}</style><img src="file://${svgFile}" width="${size}" height="${size}">`);
	await page.setViewportSize({ width: size, height: size });
	await page.goto('file://' + html);
	await page.waitForFunction(() => document.querySelector('img').complete);
	return page.screenshot({ omitBackground: true, clip: { x: 0, y: 0, width: size, height: size } });
}

// ICO holding PNG images (supported by every browser that reads favicon.ico).
function ico(pngs) {
	const head = Buffer.alloc(6);
	head.writeUInt16LE(0, 0);
	head.writeUInt16LE(1, 2);
	head.writeUInt16LE(pngs.length, 4);
	let offset = 6 + 16 * pngs.length;
	const dir = pngs.map(({ size, data }) => {
		const e = Buffer.alloc(16);
		e.writeUInt8(size >= 256 ? 0 : size, 0);
		e.writeUInt8(size >= 256 ? 0 : size, 1);
		e.writeUInt16LE(1, 4); // colour planes
		e.writeUInt16LE(32, 6); // bits per pixel
		e.writeUInt32LE(data.length, 8);
		e.writeUInt32LE(offset, 12);
		offset += data.length;
		return e;
	});
	return Buffer.concat([head, ...dir, ...pngs.map((p) => p.data)]);
}

const browser = await chromium.launch();
try {
	const page = await browser.newPage({ deviceScaleFactor: 1 });
	const small = src('pad-mark-small.svg');
	const shaped = src('pad-app-icon-shaped.svg');
	const full = src('pad-app-icon.svg');
	const files = {
		'favicon-16x16.png': await render(page, small, 16),
		'favicon-32x32.png': await render(page, small, 32),
		'icon-192.png': await render(page, shaped, 192),
		'icon-512.png': await render(page, shaped, 512),
		'apple-touch-icon.png': await render(page, full, 180),
		'padicon.png': await render(page, shaped, 512)
	};
	files['favicon.ico'] = ico([
		{ size: 16, data: files['favicon-16x16.png'] },
		{ size: 32, data: files['favicon-32x32.png'] },
		{ size: 48, data: await render(page, small, 48) }
	]);
	if (withOG) {
		const dir = mkdtempSync(join(tmpdir(), 'pad-og-'));
		const html = join(dir, 'og.html');
		writeFileSync(html, `<!doctype html><meta charset="utf-8"><style>
html,body{margin:0}
body{width:1200px;height:630px;background:radial-gradient(900px 520px at 20% 10%,#1d2147,#10122a 70%);display:flex;align-items:center;gap:60px;padding:0 100px;box-sizing:border-box;
font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","Helvetica Neue",Arial,sans-serif}
img{width:240px;height:240px;display:block}
.w{font-size:124px;font-weight:800;color:#fff;letter-spacing:-.035em;line-height:1}
.t{font-size:40px;color:#b7b7c7;margin-top:20px;line-height:1.25}
</style><img src="file://${shaped}"><div><div class="w">pad</div><div class="t">Project management<br>for the agent era</div></div>`);
		await page.setViewportSize({ width: 1200, height: 630 });
		await page.goto('file://' + html);
		await page.waitForFunction(() => document.querySelector('img').complete);
		files['og-card.png'] = await page.screenshot({ clip: { x: 0, y: 0, width: 1200, height: 630 } });
	}
	for (const [name, data] of Object.entries(files)) {
		writeFileSync(join(OUT, name), data);
		console.log('wrote', join(OUT, name));
	}
	copyFileSync(small, join(OUT, 'icon.svg'));
	copyFileSync(src('pad-mark.svg'), join(OUT, 'pad-mark.svg'));
	console.log('wrote', join(OUT, 'icon.svg'), 'and', join(OUT, 'pad-mark.svg'));
	// A sanity line: the file is what the master says, not a stale copy.
	if (!readFileSync(join(OUT, 'icon.svg'), 'utf8').includes('pixel-aligned at 16px')) throw new Error('icon.svg is not pad-mark-small');
} finally {
	await browser.close();
}
