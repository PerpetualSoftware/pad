import { beforeEach, expect, it, vi } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { join } from 'node:path';

const sdk = vi.hoisted(() => ({ callServerTool: vi.fn(), connect: vi.fn(), instances: [] as Array<Record<string, any>> }));
// Split the specifier so the production-import source guard also scans this file.
vi.mock('@modelcontextprotocol/' + 'ext-apps', () => ({ App: class {
	callServerTool = sdk.callServerTool;
	connect = sdk.connect;
	constructor() { sdk.instances.push(this); }
} }));
import { connectApp, ALLOWED_CALLS } from './bridge';

beforeEach(() => { sdk.instances.length = 0; vi.clearAllMocks(); });
it('installs handlers before connecting and forwards host notifications', async () => {
	const handlers = { onToolInput: vi.fn(), onToolResult: vi.fn(), onHostContext: vi.fn() };
	sdk.connect.mockImplementationOnce(async () => {
		const app = sdk.instances[0];
		app.ontoolinput({ arguments: { action: 'get' } });
		app.ontoolinput({});
		app.ontoolresult({ structuredContent: { id: '1' } });
		app.onhostcontextchanged({ theme: 'dark' });
	});
	await connectApp(handlers);
	expect(handlers.onToolInput).toHaveBeenNthCalledWith(1, { action: 'get' });
	expect(handlers.onToolInput).toHaveBeenNthCalledWith(2, {});
	expect(handlers.onToolResult).toHaveBeenCalledWith({ structuredContent: { id: '1' } });
	expect(handlers.onHostContext).toHaveBeenCalledWith({ theme: 'dark' });
	expect(sdk.connect).toHaveBeenCalledOnce();
});
it('refuses all calls outside the frozen allowlist before reaching the SDK', async () => {
	const bridge = await connectApp({ onToolInput() {}, onToolResult() {} });
	for (const [tool, args] of [['pad_item', { action: 'delete' }], ['pad_item', {}], ['pad_project', { action: 'next' }]] as const) {
		await expect(bridge.call(tool, args)).rejects.toThrow(`tool not allowed: ${tool}:${'action' in args ? args.action : undefined}`);
	}
	expect(sdk.callServerTool).not.toHaveBeenCalled();
	expect(Object.isFrozen(ALLOWED_CALLS)).toBe(true);
});
it('forwards every allowed pair and preserves the result', async () => {
	expect(ALLOWED_CALLS).toEqual(['pad_item:get', 'pad_item:update', 'pad_collection:list', 'pad_board:show', 'pad_board:changes']);
	const bridge = await connectApp({ onToolInput() {}, onToolResult() {} });
	const result = { structuredContent: { ok: true } }; sdk.callServerTool.mockResolvedValue(result);
	for (const pair of ALLOWED_CALLS) {
		const [tool, action] = pair.split(':'); const args = { action, workspace: 'demo' };
		expect(await bridge.call(tool, args)).toBe(result);
		expect(sdk.callServerTool).toHaveBeenLastCalledWith({ name: tool, arguments: args });
	}
	expect(sdk.callServerTool).toHaveBeenCalledTimes(5);
});
it('keeps the app source within its bridge and rendering boundaries', () => {
	const root = fileURLToPath(new URL('.', import.meta.url));
	const walk = (dir: string): string[] => readdirSync(dir, { withFileTypes: true }).flatMap(entry => entry.isDirectory() ? walk(join(dir, entry.name)) : [join(dir, entry.name)]);
	const banned = ['{@' + 'html', 'open' + 'Link', 'send' + 'Message', 'update' + 'ModelContext', 'fet' + 'ch(', 'XML' + 'HttpRequest', 'Web' + 'Socket', 'Event' + 'Source'];
	for (const path of walk(root)) {
		const source = readFileSync(path, 'utf8');
		for (const token of banned) expect(source, path).not.toContain(token);
		if (path !== join(root, 'bridge.ts')) expect(source, path).not.toContain('@modelcontextprotocol/' + 'ext-apps');
	}
});
