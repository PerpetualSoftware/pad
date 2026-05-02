import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from '../fixtures';

/**
 * Shared seeding helpers for screenshot specs.
 *
 * Two consumers today:
 *   - e2e/screenshots.spec.ts          (README hero shots)
 *   - e2e/blog-screenshots.spec.ts     (per-blog-post shots)
 *
 * Each helper hits the same REST endpoints the web UI uses, so the
 * resulting items behave identically to ones a human creates via the
 * dashboard.
 */

// `fields` is a JSON-encoded string on the wire (see
// internal/models/item.go ItemCreate). Stringify it once.
const enc = (obj: Record<string, unknown>) => JSON.stringify(obj);

function authHeaders(fixture: SuiteFixture) {
	return {
		Authorization: `Bearer ${fixture.apiToken}`,
		'Content-Type': 'application/json'
	};
}

/**
 * Realistic-looking project: one active plan, half a dozen tasks across
 * statuses + priorities, a couple of ideas. Designed to make the
 * dashboard's "Active Plans" widget show a non-zero progress bar and
 * give the board view meaningful columns.
 *
 * Idempotent enough for `reuseExistingServer: true` local re-runs:
 * each call will create *additional* items, but the screenshots only
 * care that there's *some* content. If you need a clean slate, point
 * Playwright at a fresh PAD_E2E_DATA_DIR.
 */
export async function seedRealisticContent(fixture: SuiteFixture, request: APIRequestContext) {
	const ws = fixture.workspaceSlug;
	const headers = authHeaders(fixture);

	// 1. Active plan
	const planResp = await request.post(`/api/v1/workspaces/${ws}/collections/plans/items`, {
		headers,
		data: {
			title: 'v0.2 — Collaboration',
			fields: enc({ status: 'active' }),
			content: 'Real-time multiplayer editing, team invites, and shared views.'
		}
	});
	if (!planResp.ok()) {
		throw new Error(`plan create failed (${planResp.status()}): ${await planResp.text()}`);
	}
	const plan = (await planResp.json()) as { id?: string };
	if (!plan.id) throw new Error('plan create succeeded but response missing id');

	// 2. Tasks — explicit mix of status / priority / parent linkage
	const tasks: { title: string; fields: Record<string, string>; parent?: string }[] = [
		{
			title: 'Fix OAuth redirect bug',
			fields: { status: 'in-progress', priority: 'high', effort: 'm' },
			parent: plan.id
		},
		{
			title: 'Add rate limiting to API endpoints',
			fields: { status: 'in-progress', priority: 'high', effort: 'l' },
			parent: plan.id
		},
		{
			title: 'Refactor auth middleware',
			fields: { status: 'open', priority: 'medium', effort: 'm' }
		},
		{
			title: 'Add SSO support for enterprise users',
			fields: { status: 'open', priority: 'medium', effort: 'xl' },
			parent: plan.id
		},
		{
			title: 'Document the deploy pipeline',
			fields: { status: 'open', priority: 'low', effort: 's' }
		},
		{
			title: 'Update Go to 1.26',
			fields: { status: 'done', priority: 'medium', effort: 's' }
		},
		{
			title: 'Set up CI security scanning',
			fields: { status: 'done', priority: 'high', effort: 's' },
			parent: plan.id
		}
	];

	for (const t of tasks) {
		const resp = await request.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, {
			headers,
			data: {
				title: t.title,
				fields: enc(t.fields),
				...(t.parent ? { parent_id: t.parent } : {})
			}
		});
		if (!resp.ok()) {
			throw new Error(`task ${t.title} failed (${resp.status()}): ${await resp.text()}`);
		}
	}

	// 3. Ideas — a couple, varied state
	const ideas: { title: string; fields: Record<string, string> }[] = [
		{ title: 'Real-time presence indicators', fields: { status: 'exploring', impact: 'high' } },
		{ title: 'Slack integration for notifications', fields: { status: 'new', impact: 'medium' } }
	];
	for (const idea of ideas) {
		const resp = await request.post(`/api/v1/workspaces/${ws}/collections/ideas/items`, {
			headers,
			data: { title: idea.title, fields: enc(idea.fields) }
		});
		if (!resp.ok()) {
			throw new Error(`idea ${idea.title} failed (${resp.status()}): ${await resp.text()}`);
		}
	}
}

export interface ConventionInput {
	title: string;
	trigger: string;
	priority: 'must' | 'should' | 'nice-to-have';
	scope?: 'all' | 'backend' | 'frontend' | 'mobile' | 'docs' | 'devops';
	role?: string;
	content?: string;
}

/**
 * Bulk-create conventions in the workspace. Mirrors the inline-create
 * flow in web/src/routes/[username]/[workspace]/conventions/+page.svelte
 * but skips the form UX.
 */
export async function seedConventions(
	fixture: SuiteFixture,
	request: APIRequestContext,
	conventions: ConventionInput[]
) {
	const ws = fixture.workspaceSlug;
	const headers = authHeaders(fixture);

	for (const c of conventions) {
		const fields: Record<string, string> = {
			status: 'active',
			trigger: c.trigger,
			priority: c.priority,
			scope: c.scope ?? 'all'
		};
		if (c.role) fields.role = c.role;

		const resp = await request.post(`/api/v1/workspaces/${ws}/collections/conventions/items`, {
			headers,
			data: {
				title: c.title,
				fields: enc(fields),
				...(c.content ? { content: c.content } : {})
			}
		});
		if (!resp.ok()) {
			throw new Error(`convention "${c.title}" failed (${resp.status()}): ${await resp.text()}`);
		}
	}
}

interface LibraryConvention {
	title: string;
	content: string;
	category: string;
	trigger: string;
	surfaces?: string[];
	enforcement: 'must' | 'should' | 'nice-to-have';
	commands?: string[];
}

interface ConventionLibraryResponse {
	categories: { name: string; conventions: LibraryConvention[] }[];
}

/**
 * Activate one or more conventions from the curated /convention-library.
 * Mirrors api.library.activate() in web/src/lib/api/client.ts — this
 * is a simple POST to the conventions collection with the library
 * convention's data (no dedicated /activate endpoint exists; the web
 * UI does the same thing).
 *
 * Pass library titles exactly as they appear in `pad library list`.
 */
export async function activateLibraryConventions(
	fixture: SuiteFixture,
	request: APIRequestContext,
	titles: string[]
) {
	const ws = fixture.workspaceSlug;
	const headers = authHeaders(fixture);

	const libraryResp = await request.get('/api/v1/convention-library', { headers });
	if (!libraryResp.ok()) {
		throw new Error(`library fetch failed (${libraryResp.status()}): ${await libraryResp.text()}`);
	}
	const library = (await libraryResp.json()) as ConventionLibraryResponse;
	const byTitle = new Map<string, LibraryConvention>();
	for (const cat of library.categories) {
		for (const conv of cat.conventions) byTitle.set(conv.title, conv);
	}

	for (const title of titles) {
		const conv = byTitle.get(title);
		if (!conv) {
			const available = [...byTitle.keys()].slice(0, 10).join(', ');
			throw new Error(`library convention "${title}" not found. First few available: ${available}`);
		}

		const resp = await request.post(`/api/v1/workspaces/${ws}/collections/conventions/items`, {
			headers,
			data: {
				title: conv.title,
				content: conv.content,
				fields: enc({
					status: 'active',
					category: conv.category,
					trigger: conv.trigger,
					scope: conv.surfaces?.[0] ?? 'all',
					priority: conv.enforcement,
					enforcement: conv.enforcement,
					surfaces: conv.surfaces,
					commands: conv.commands ?? [],
					convention: {
						category: conv.category,
						trigger: conv.trigger,
						surfaces: conv.surfaces,
						enforcement: conv.enforcement,
						commands: conv.commands ?? []
					}
				})
			}
		});
		if (!resp.ok()) {
			throw new Error(`activate "${title}" failed (${resp.status()}): ${await resp.text()}`);
		}
	}
}
