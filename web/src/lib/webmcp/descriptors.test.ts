import { describe, it, expect } from 'vitest';
import type { ToolSurfaceTool, ToolSurfaceResponse } from '$lib/api/client';
import { buildDescriptor, buildDescriptors, isAllReadOnly } from './descriptors';

// A no-op execute closure — the builder is pure aside from carrying it through.
const noopExecute = async (): Promise<ModelContextToolResult> => ({
	content: [{ type: 'text', text: '' }],
});

// pad_search: all-read tool that ALSO declares a `workspace` param — the
// canonical DR-4 + DR-2 case.
const padSearch: ToolSurfaceTool = {
	name: 'pad_search',
	description: 'Search items.',
	workspace: true,
	actions: [{ name: 'query', read_only: true }],
	params: [
		{ name: 'action', type: 'string', enum: ['query'] },
		{ name: 'workspace', type: 'string', description: 'Workspace slug.' },
		{ name: 'query', type: 'string', description: 'The search query.' },
		{ name: 'limit', type: 'number' },
	],
};

// pad_item: MIXED read + write tool. readOnlyHint must be ABSENT.
const padItem: ToolSurfaceTool = {
	name: 'pad_item',
	description: 'Manage items.',
	workspace: true,
	actions: [
		{ name: 'list', read_only: true },
		{ name: 'get', read_only: true },
		{ name: 'create', read_only: false },
		{ name: 'delete', read_only: false },
	],
	params: [
		{ name: 'action', type: 'string', enum: ['list', 'get', 'create', 'delete'] },
		{ name: 'workspace', type: 'string' },
		{ name: 'ref', type: 'ref' },
		{ name: 'title', type: 'string' },
	],
};

describe('isAllReadOnly (DR-2)', () => {
	it('true when every action is read_only', () => {
		expect(isAllReadOnly(padSearch)).toBe(true);
	});

	it('false for a mixed read/write tool', () => {
		expect(isAllReadOnly(padItem)).toBe(false);
	});

	it('false when the tool has zero actions (conservative)', () => {
		expect(isAllReadOnly({ ...padSearch, actions: [] })).toBe(false);
	});
});

describe('buildDescriptor — workspace strip (DR-4)', () => {
	it('strips the workspace param from the inputSchema', () => {
		const { tool } = buildDescriptor(padSearch, noopExecute);
		expect(tool.inputSchema.properties).toBeDefined();
		expect('workspace' in (tool.inputSchema.properties ?? {})).toBe(false);
		// other params survive
		expect('query' in (tool.inputSchema.properties ?? {})).toBe(true);
		expect('action' in (tool.inputSchema.properties ?? {})).toBe(true);
	});

	it('records hasWorkspaceParam when the source declared one', () => {
		const { hasWorkspaceParam } = buildDescriptor(padSearch, noopExecute);
		expect(hasWorkspaceParam).toBe(true);
	});

	it('keeps action required and leaves other params optional', () => {
		const { tool } = buildDescriptor(padSearch, noopExecute);
		expect(tool.inputSchema.required).toEqual(['action']);
	});

	it('strips workspace even for a mixed tool', () => {
		const { tool } = buildDescriptor(padItem, noopExecute);
		expect('workspace' in (tool.inputSchema.properties ?? {})).toBe(false);
		expect('ref' in (tool.inputSchema.properties ?? {})).toBe(true);
	});
});

describe('buildDescriptor — readOnlyHint (DR-2)', () => {
	it('sets readOnlyHint=true for an all-read tool', () => {
		const { tool } = buildDescriptor(padSearch, noopExecute);
		expect(tool.annotations?.readOnlyHint).toBe(true);
	});

	it('omits annotations entirely for a mixed tool', () => {
		const { tool } = buildDescriptor(padItem, noopExecute);
		// No readOnlyHint at all → the host prompts for per-invocation consent.
		expect(tool.annotations).toBeUndefined();
	});
});

describe('buildDescriptor — param types & metadata', () => {
	it('maps `flag` → boolean and `number` → number; ref/enum → string', () => {
		const tool: ToolSurfaceTool = {
			name: 'pad_x',
			description: 'x',
			workspace: false,
			actions: [{ name: 'go', read_only: true }],
			params: [
				{ name: 'action', type: 'string', enum: ['go'] },
				{ name: 'force', type: 'flag' },
				{ name: 'count', type: 'number' },
				{ name: 'ref', type: 'ref' },
			],
		};
		const { tool: built } = buildDescriptor(tool, noopExecute);
		const props = built.inputSchema.properties as Record<string, { type: string }>;
		expect(props.force.type).toBe('boolean');
		expect(props.count.type).toBe('number');
		expect(props.ref.type).toBe('string');
	});

	it('carries the action enum through', () => {
		const { tool } = buildDescriptor(padItem, noopExecute);
		const props = tool.inputSchema.properties as Record<string, { enum?: string[] }>;
		expect(props.action.enum).toEqual(['list', 'get', 'create', 'delete']);
	});

	it('exposes the action read-only map for the dispatcher', () => {
		const { actions } = buildDescriptor(padItem, noopExecute);
		expect(actions.get('get')).toBe(true);
		expect(actions.get('create')).toBe(false);
	});
});

describe('buildDescriptors', () => {
	it('builds one descriptor per tool and wires executeFor by name', () => {
		const surface: ToolSurfaceResponse = {
			tool_surface_version: '0.7',
			tools: [padSearch, padItem],
		};
		const seen: string[] = [];
		const built = buildDescriptors(surface, (name) => {
			seen.push(name);
			return noopExecute;
		});
		expect(built.map((b) => b.tool.name)).toEqual(['pad_search', 'pad_item']);
		expect(seen).toEqual(['pad_search', 'pad_item']);
	});
});
