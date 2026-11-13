import { App, applyDocumentTheme, applyHostFonts, applyHostStyleVariables, type McpUiHostContext } from '@modelcontextprotocol/ext-apps';
import type { ToolResultLike } from './model';
export type { ToolResultLike } from './model';

export const ALLOWED_CALLS = Object.freeze([
	'pad_item:get', 'pad_item:update', 'pad_collection:list', 'pad_board:show', 'pad_board:changes'
] as const);

/** What the host tells the view about itself: theme, style variables, safe area, locale. */
export type HostContext = McpUiHostContext;

// The SDK's document helpers live here because this is the only file that may import it.
function applyHostStyle(ctx: HostContext): void {
	if (ctx.theme) applyDocumentTheme(ctx.theme);
	if (ctx.styles?.variables) applyHostStyleVariables(ctx.styles.variables);
	if (ctx.styles?.css?.fonts) applyHostFonts(ctx.styles.css.fonts);
}

export interface Bridge {
	call(tool: string, args: Record<string, unknown>): Promise<ToolResultLike>;
}

export async function connectApp(handlers: {
	onToolInput(args: Record<string, unknown>): void;
	onToolResult(result: ToolResultLike): void;
	onHostContext?(ctx: HostContext): void;
}, name = 'Pad'): Promise<Bridge> {
	const app = new App({ name, version: '1.0.0' }, {});
	// Register before connecting: hosts can deliver initial data during the handshake.
	app.ontoolinput = params => handlers.onToolInput(params.arguments ?? {});
	app.ontoolresult = result => handlers.onToolResult(result);
	const hostContext = (ctx: HostContext) => { applyHostStyle(ctx); handlers.onHostContext?.(ctx); };
	app.onhostcontextchanged = hostContext;
	await app.connect();
	// Change notifications are partial and only follow the handshake; the initial context is read once here.
	const initial = app.getHostContext();
	if (initial) hostContext(initial);
	return {
		async call(tool, args) {
			const pair = `${tool}:${args.action}`;
			if (typeof args.action !== 'string' || !ALLOWED_CALLS.some(allowed => allowed === pair)) throw new Error(`tool not allowed: ${pair}`);
			return app.callServerTool({ name: tool, arguments: args });
		}
	};
}
