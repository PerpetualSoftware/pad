import { App } from '@modelcontextprotocol/ext-apps';
import type { ToolResultLike } from './model';
export type { ToolResultLike } from './model';

export const ALLOWED_CALLS = Object.freeze([
	'pad_item:get', 'pad_item:update', 'pad_collection:list', 'pad_board:show', 'pad_board:changes'
] as const);

export interface Bridge {
	call(tool: string, args: Record<string, unknown>): Promise<ToolResultLike>;
}

export async function connectApp(handlers: {
	onToolInput(args: Record<string, unknown>): void;
	onToolResult(result: ToolResultLike): void;
	onHostContext?(ctx: unknown): void;
}): Promise<Bridge> {
	const app = new App({ name: 'Pad item card', version: '1.0.0' }, {});
	// Register before connecting: hosts can deliver initial data during the handshake.
	app.ontoolinput = params => handlers.onToolInput(params.arguments ?? {});
	app.ontoolresult = result => handlers.onToolResult(result);
	app.onhostcontextchanged = ctx => handlers.onHostContext?.(ctx);
	await app.connect();
	return {
		async call(tool, args) {
			const pair = `${tool}:${args.action}`;
			if (typeof args.action !== 'string' || !ALLOWED_CALLS.some(allowed => allowed === pair)) throw new Error(`tool not allowed: ${pair}`);
			return app.callServerTool({ name: tool, arguments: args });
		}
	};
}
