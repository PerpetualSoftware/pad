// Build one self-contained HTML MCP App, alongside the materializer output.
import { build, transform } from 'esbuild';
import { compile, compileModule } from 'svelte/compiler';
import path from 'node:path';
import fs from 'node:fs';
import { fileURLToPath } from 'node:url';

const web = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const entry = path.join(web, 'src/mcp-app/card.ts');
const outfile = path.join(web, 'build-mcp-app/item-card.html');

try {
	if (!fs.existsSync(entry)) throw new Error(`entry file missing: ${entry}`);
	const result = await build({
		entryPoints: [entry],
		bundle: true,
		format: 'iife',
		platform: 'browser',
		conditions: ['svelte', 'browser'],
		target: 'es2022',
		minify: true,
		legalComments: 'none',
		write: false,
		define: { 'process.env.NODE_ENV': '"production"' },
		plugins: [{
			// The SDK and its MCP dependencies import zod as a namespace, which keeps
			// `z.locales` and so every error-message translation (about 260 kB, half
			// the size cap) alive. Nothing here reads them: zod configures English from
			// locales/en.js directly, and that is the only one this index keeps.
			name: 'zod-english-only',
			setup(bundler) {
				bundler.onLoad({ filter: /[\\/]zod[\\/]v4[\\/]locales[\\/]index\.js$/ }, ({ path: filename }) => ({
					contents: 'export { default as en } from "./en.js";', loader: 'js', resolveDir: path.dirname(filename)
				}));
			}
		}, {
			name: 'svelte',
			setup(bundler) {
				bundler.onLoad({ filter: /\.svelte(?:\.[jt]s)?$/ }, async ({ path: filename }) => {
					let source = fs.readFileSync(filename, 'utf8');
					let compiled;
					if (filename.endsWith('.svelte')) {
						compiled = compile(source, { filename, generate: 'client', css: 'injected' });
					} else {
						if (filename.endsWith('.ts')) source = (await transform(source, { loader: 'ts', target: 'esnext' })).code;
						compiled = compileModule(source, { filename, generate: 'client' });
					}
					return { contents: compiled.js.code, loader: 'js', resolveDir: path.dirname(filename) };
				});
			}
		}],
		logLevel: 'warning'
	});
	const bundle = result.outputFiles[0].text.replace(/<\/script/gi, match => '<\\/' + match.slice(2));
	// Inside an inline script, '<!--' followed later by '<script' puts the HTML
	// parser in the double-escaped state, where '</script>' no longer ends the
	// script. Escaping either is unsafe in regex literals, so refuse instead.
	if (/<script/i.test(bundle)) throw new Error('bundle contains "<script", which can break the inline script tag');
	const template = fs.readFileSync(path.join(web, 'src/mcp-app/template.html'), 'utf8');
	if (!template.includes('<!--BUNDLE-->')) throw new Error('template is missing <!--BUNDLE-->');
	const html = template.replace('<!--BUNDLE-->', () => `<script>${bundle}</script>`);
	const bytes = Buffer.byteLength(html);
	if (bytes > 512 * 1024) throw new Error(`output exceeds 512 KiB (${bytes} bytes)`);
	if (/(?:src|href)=["']http/i.test(html)) throw new Error('output contains an external HTTP asset URL');
	fs.mkdirSync(path.dirname(outfile), { recursive: true });
	fs.writeFileSync(outfile, html);
	console.log(`build-mcp-app: wrote ${path.relative(process.cwd(), outfile)} (${bytes} bytes)`);
} catch (error) {
	console.error(`build-mcp-app: ${error.message}`);
	process.exit(1);
}
