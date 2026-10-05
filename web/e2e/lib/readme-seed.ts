import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from '../fixtures';

/**
 * Seed for the README / getpad.dev product screenshots (TASK-3426).
 *
 * Separate from ./demo-data.ts on purpose: that file is mirrored by
 * pad-remotion, and these shots want a richer, screenshot-shaped workspace
 * (a renamed workspace and author, a plan whose children are LINKED so its
 * progress bar reads non-zero, agent-attributed writes, comments, a doc).
 *
 * Runs only inside screenshots.spec.ts, against the e2e fixture's own
 * throwaway workspace. Never point it at a real workspace (CONVE-15).
 */

const enc = (obj: Record<string, unknown>) => JSON.stringify(obj);

interface Created {
	id: string;
	slug: string;
}

export interface ReadmeSeed {
	planSlug: string;
	featureTaskSlug: string;
}

/** The agent name write requests carry, so the activity feed shows agent work. */
export const README_AGENT = 'Claude Code';

export async function seedReadmeShowcase(
	fixture: SuiteFixture,
	request: APIRequestContext
): Promise<ReadmeSeed> {
	const ws = fixture.workspaceSlug;
	const human = {
		Authorization: `Bearer ${fixture.apiToken}`,
		'Content-Type': 'application/json'
	};
	const agent = { ...human, 'X-Pad-Agent': README_AGENT };

	const must = async (what: string, resp: Awaited<ReturnType<APIRequestContext['post']>>) => {
		if (!resp.ok()) throw new Error(`${what} failed (${resp.status()}): ${await resp.text()}`);
		return resp;
	};

	// The fixture names its workspace and admin "E2E …"; give the shots a
	// project and a person instead.
	await must(
		'rename workspace',
		await request.patch(`/api/v1/workspaces/${ws}`, { headers: human, data: { name: 'Atlas' } })
	);
	await must(
		'rename admin',
		await request.patch('/api/v1/auth/me', { headers: human, data: { name: 'Maya Chen' } })
	);

	const create = async (
		collection: string,
		title: string,
		fields: Record<string, unknown>,
		opts: { content?: string; asAgent?: boolean } = {}
	): Promise<Created> => {
		const resp = await must(
			`${collection} "${title}"`,
			await request.post(`/api/v1/workspaces/${ws}/collections/${collection}/items`, {
				headers: opts.asAgent ? agent : human,
				data: {
					title,
					fields: enc(fields),
					...(opts.content ? { content: opts.content } : {})
				}
			})
		);
		return (await resp.json()) as Created;
	};
	const update = async (slug: string, patch: Record<string, unknown>, asAgent = false) => {
		await must(
			`update ${slug}`,
			await request.patch(`/api/v1/workspaces/${ws}/items/${slug}`, {
				headers: asAgent ? agent : human,
				data: patch
			})
		);
	};
	const comment = async (slug: string, body: string, asAgent = false) => {
		await must(
			`comment on ${slug}`,
			await request.post(`/api/v1/workspaces/${ws}/items/${slug}/comments`, {
				headers: asAgent ? agent : human,
				data: { body }
			})
		);
	};

	const plan = await create(
		'plans',
		'v0.4 — Team workspaces',
		{ status: 'active' },
		{
			content: [
				'Make Atlas usable by a whole team, not just one developer.',
				'',
				'## Goals',
				'- Invite teammates with per-collection access',
				'- Show who changed what, and whether a person or an agent did it',
				'- Keep agent sessions in sync with the web UI in real time'
			].join('\n')
		}
	);

	// [title, status, priority, effort, linked to the plan, written by the agent]
	const tasks: [string, string, string, string, boolean, boolean][] = [
		['Invite teammates by email', 'done', 'high', 'm', true, false],
		['Per-collection access for members', 'done', 'high', 'l', true, true],
		['Activity feed with author attribution', 'done', 'medium', 'm', true, true],
		['Real-time sync between agent and web UI', 'in-progress', 'high', 'l', true, true],
		['Workspace switcher in the top bar', 'done', 'medium', 's', true, false],
		['Audit log export', 'open', 'medium', 'm', true, false],
		['Rate-limit invite emails', 'open', 'low', 's', true, true],
		['Fix OAuth redirect on custom domains', 'in-progress', 'critical', 's', false, false],
		['Upgrade to Go 1.26', 'done', 'low', 'xs', false, true],
		['Document the self-host Docker setup', 'open', 'medium', 's', false, false]
	];
	const created: Record<string, Created> = {};
	for (const [title, status, priority, effort, linked, asAgent] of tasks) {
		created[title] = await create(
			'tasks',
			title,
			{ status: 'open', priority, effort, ...(linked ? { parent: plan.id } : {}) },
			{ asAgent }
		);
		// Created open, then moved, so the feed shows status changes rather
		// than a wall of "Created".
		if (status !== 'open') await update(created[title].slug, { fields_patch: { status } }, asAgent);
	}

	const feature = created['Real-time sync between agent and web UI'];
	const me = (await (await must('whoami', await request.get('/api/v1/auth/me', { headers: human }))).json()) as {
		id: string;
	};
	await update(feature.slug, { assigned_user_id: me.id });
	await update(
		feature.slug,
		{
			content: [
				'When an agent updates an item, every open browser tab should show it within a second, and the reverse.',
				'',
				'## Approach',
				'- Server-sent events per workspace, filtered by what the viewer can see',
				'- The web client keeps a local index and applies deltas by sequence number',
				'- A dropped connection resyncs from the last cursor instead of reloading',
				'',
				'## Done when',
				'- [x] SSE stream with per-viewer filtering',
				'- [x] Delta endpoint keyed on a workspace sequence',
				'- [ ] Resync after a revoked grant',
				'- [ ] Load test with 50 open tabs'
			].join('\n')
		},
		true
	);
	await comment(
		feature.slug,
		'Delta endpoint is in. Next is the resync path when someone loses access mid-session.',
		true
	);
	await comment(feature.slug, 'Looks good. Can we cover the 50-tab case before we ship?');

	await create(
		'docs',
		'Architecture overview',
		{ status: 'published' },
		{
			content: [
				'Atlas is a single Go binary with an embedded web UI and SQLite storage.',
				'',
				'## Components',
				'- **API**: REST under `/api/v1`',
				'- **Web**: SvelteKit, served from the binary',
				'- **Agents**: the same API, through the CLI or MCP'
			].join('\n')
		}
	);
	await create('ideas', 'Slack notifications for blocked work', { status: 'exploring', impact: 'high' });
	await create('ideas', 'Weekly digest email', { status: 'new', impact: 'medium' }, { asAgent: true });

	return { planSlug: plan.slug, featureTaskSlug: feature.slug };
}
