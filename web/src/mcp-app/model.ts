export const CARD_ACTIONS = ['get', 'create', 'update', 'move', 'restore', 'claim', 'release'] as const;

export interface ItemLike {
	id: string;
	title: string;
	seq: number;
	collection_slug: string;
	ref?: string;
	content?: string;
	fields?: Record<string, unknown>;
}

export interface ToolResultLike {
	isError?: boolean;
	structuredContent?: unknown;
	content?: readonly { type: string; text?: string }[];
}

function isObject(x: unknown): x is Record<string, unknown> {
	return x !== null && typeof x === 'object' && !Array.isArray(x);
}

export function isItemResult(x: unknown): x is ItemLike {
	return isObject(x) && typeof x.id === 'string' && typeof x.title === 'string'
		&& typeof x.seq === 'number' && typeof x.collection_slug === 'string';
}

export function shouldRenderCard(toolInputArgs: unknown, structuredContent: unknown): boolean {
	return isObject(toolInputArgs) && CARD_ACTIONS.some(action => action === toolInputArgs.action)
		&& isItemResult(structuredContent);
}

export interface SchemaField {
	key: string;
	label?: string;
	type: string;
	options?: string[];
	computed?: boolean;
}

export function editableFields(collectionSchema: { fields: SchemaField[] }, item: ItemLike) {
	// Computed is a boolean on FieldDef: internal/models/collection.go:26.
	return collectionSchema.fields.filter(field => field.type === 'select' && !field.computed)
		.map(field => ({ ...field, options: field.options ?? [], value: item.fields?.[field.key] }));
}

export function buildUpdateArgs(input: {
	workspace?: string;
	ref: string;
	title?: string;
	field?: { key: string; value: string };
	expectedSeq: number;
}): Record<string, unknown> {
	// Wire names: internal/mcp/catalog_item.go:340 (field array), :403 (expected_seq).
	return {
		action: 'update', ref: input.ref, expected_seq: input.expectedSeq,
		...(input.workspace !== undefined ? { workspace: input.workspace } : {}),
		...(input.title !== undefined ? { title: input.title } : {}),
		...(input.field ? { field: [`${input.field.key}=${input.field.value}`] } : {})
	};
}

function envelopeError(value: unknown): { code: string; message: string } | undefined {
	if (!isObject(value) || !isObject(value.error)) return;
	const { code, message } = value.error;
	if (typeof code === 'string' && typeof message === 'string') return { code, message };
}

export function parseToolError(result: ToolResultLike): { code: string; message: string } | undefined {
	if (!result.isError) return;
	// NewErrorResult emits {error:{code,message,...}} in both channels:
	// internal/mcp/errors.go:353. This preserves update_conflict for refresh/retry.
	const structured = envelopeError(result.structuredContent);
	if (structured) return structured;
	const texts = result.content?.filter(block => block.type === 'text' && typeof block.text === 'string') ?? [];
	for (const block of texts) {
		try {
			const error = envelopeError(JSON.parse(block.text!));
			if (error) return error;
		} catch { /* Plain text errors also have a displayable fallback. */ }
	}
	return { code: 'unknown', message: texts.map(block => block.text).join('\n') || 'Tool call failed' };
}

// Return text for normal escaped text rendering by the view.
export function contentPreview(content: string | undefined, max = 400): string {
	const text = content ?? '';
	return text.length > max ? text.slice(0, max) + '…' : text;
}
