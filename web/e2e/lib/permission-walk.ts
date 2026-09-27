import { request, type APIRequestContext, type BrowserContext } from '@playwright/test';
import { suiteFixture, quietCrossActorToasts } from '../fixtures';

/**
 * Multi-account permission-affordance walk: the fixture (TASK-2866 U1).
 *
 * HT-1157 is BUG-984's quality gate. It walks one workspace as each effective
 * permission shape and checks that no forbidden affordance renders. This module
 * seeds that world: a workspace of its own and one account per shape, each
 * holding its own API token.
 *
 * Why its own workspace: specs on the shared e2e workspace starve under load
 * (the delta-sync precedent), and the walk mutates membership and grants.
 * Why a token per account, not a session: sessions are User-Agent bound
 * (see fixtures.ts), so a session minted in node is refused by the browser.
 * Each account logs in ONCE from node, only to mint that token, because
 * minting requires an interactive session (BUG-2890).
 *
 * Every name carries a per-seed tag, so a rerun against a reused server, or
 * a second worker seeding its own world, never collides.
 */

export type AccountKey =
	| 'owner'
	| 'editor'
	| 'viewer'
	| 'viewerTasksEdit'
	| 'guestItemEdit'
	| 'guestPrecedence'
	| 'editorSpecific';

/** The HT-1157 shapes, in its order (the 7th is its optional case). */
export const ACCOUNT_KEYS: readonly AccountKey[] = [
	'owner',
	'editor',
	'viewer',
	'viewerTasksEdit',
	'guestItemEdit',
	'guestPrecedence',
	'editorSpecific'
];

export interface WalkAccount {
	key: AccountKey;
	email: string;
	username: string;
	userId: string;
	token: string;
}

export interface WalkItem {
	slug: string;
	ref: string;
	title: string;
}

export interface PermissionWalk {
	baseURL: string;
	workspaceSlug: string;
	/** Web routes are /{owner username}/{workspace}, whoever is viewing. */
	workspacePath: string;
	accounts: Record<AccountKey, WalkAccount>;
	tasks: WalkItem[];
	ideas: WalkItem[];
	/**
	 * The task both guests are granted: item-EDIT for guestItemEdit, and
	 * item-VIEW for guestPrecedence, whose collection-EDIT grant on Tasks
	 * must lose to it (the precedence case).
	 */
	grantedTask: WalkItem;
}

const PASSWORD = 'Playwright-Walk-2026!';

async function expectOk(resp: Awaited<ReturnType<APIRequestContext['get']>>, what: string) {
	if (!resp.ok()) {
		throw new Error(`permission-walk seed: ${what} failed (${resp.status()}): ${await resp.text()}`);
	}
	return resp;
}

function bearer(baseURL: string, token: string) {
	return request.newContext({ baseURL, extraHTTPHeaders: { Authorization: `Bearer ${token}` } });
}

async function mintAccount(
	baseURL: string,
	admin: APIRequestContext,
	key: AccountKey,
	tag: string
): Promise<WalkAccount> {
	const username = `walk${key.toLowerCase()}${tag}`;
	const email = `${username}@example.com`;
	// Admin-created accounts are verified, so they can log in at once.
	await expectOk(
		await admin.post('/api/v1/auth/register', {
			data: { email, username, name: `Walk ${key}`, password: PASSWORD }
		}),
		`register ${key}`
	);

	const session = await request.newContext({ baseURL });
	try {
		await expectOk(
			await session.post('/api/v1/auth/login', { data: { email, password: PASSWORD } }),
			`login ${key}`
		);
		const state = await session.storageState();
		const csrf = state.cookies.find((c) => c.name === 'pad_csrf' || c.name === '__Host-pad_csrf');
		if (!csrf) throw new Error(`permission-walk seed: login ${key} issued no pad_csrf cookie`);
		// No workspace_id: guests belong to no workspace, and a token scoped
		// to one would decide what the walk measures.
		const tokenResp = await expectOk(
			await session.post('/api/v1/auth/tokens', {
				headers: { 'X-CSRF-Token': csrf.value },
				data: { name: `walk-${key}`, expires_in: 1 }
			}),
			`token ${key}`
		);
		const { token } = (await tokenResp.json()) as { token?: string };
		const me = (await (await expectOk(await session.get('/api/v1/auth/me'), `me ${key}`)).json()) as {
			id?: string;
			username?: string;
		};
		if (!token || !me.id || !me.username) {
			throw new Error(`permission-walk seed: ${key} is missing a token, id or username`);
		}
		return { key, email, username: me.username, userId: me.id, token };
	} finally {
		await session.dispose();
	}
}

async function createItems(
	owner: APIRequestContext,
	ws: string,
	collection: string,
	titles: string[]
): Promise<WalkItem[]> {
	const items: WalkItem[] = [];
	for (const title of titles) {
		const resp = await expectOk(
			await owner.post(`/api/v1/workspaces/${ws}/collections/${collection}/items`, {
				data: { title }
			}),
			`create ${collection} item`
		);
		const item = (await resp.json()) as { slug: string; ref?: string };
		items.push({ slug: item.slug, ref: item.ref ?? '', title });
	}
	return items;
}

/** Seed one walk world. Each call builds a fresh, independent one. */
export async function seedPermissionWalk(): Promise<PermissionWalk> {
	const { baseURL, apiToken } = suiteFixture();
	const tag = `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`;

	const admin = await bearer(baseURL, apiToken);
	const accounts = {} as Record<AccountKey, WalkAccount>;
	try {
		for (const key of ACCOUNT_KEYS) {
			accounts[key] = await mintAccount(baseURL, admin, key, tag);
		}
	} finally {
		await admin.dispose();
	}

	const owner = await bearer(baseURL, accounts.owner.token);
	try {
		const wsResp = await expectOk(
			await owner.post('/api/v1/workspaces', {
				data: { name: `Walk ${tag}`, slug: `walk-${tag}`, template: 'startup' }
			}),
			'create workspace'
		);
		const ws = ((await wsResp.json()) as { slug: string }).slug;

		const tasks = await createItems(owner, ws, 'tasks', ['Walk task A', 'Walk task B', 'Walk task C']);
		const ideas = await createItems(owner, ws, 'ideas', ['Walk idea A', 'Walk idea B']);
		const grantedTask = tasks[0];

		const members: [AccountKey, 'editor' | 'viewer'][] = [
			['editor', 'editor'],
			['viewer', 'viewer'],
			['viewerTasksEdit', 'viewer'],
			['editorSpecific', 'editor']
		];
		for (const [key, role] of members) {
			await expectOk(
				await owner.post(`/api/v1/workspaces/${ws}/members/invite`, {
					data: { email: accounts[key].email, role }
				}),
				`invite ${key}`
			);
		}

		const tasksColl = (await (
			await expectOk(await owner.get(`/api/v1/workspaces/${ws}/collections/tasks`), 'get tasks')
		).json()) as { id: string };
		await expectOk(
			await owner.put(
				`/api/v1/workspaces/${ws}/members/${accounts.editorSpecific.userId}/collection-access`,
				{ data: { mode: 'specific', collection_ids: [tasksColl.id] } }
			),
			'restrict editorSpecific'
		);

		for (const key of ['viewerTasksEdit', 'guestPrecedence'] as const) {
			await expectOk(
				await owner.post(`/api/v1/workspaces/${ws}/collections/tasks/grants`, {
					data: { user_id: accounts[key].userId, permission: 'edit' }
				}),
				`collection grant ${key}`
			);
		}
		const itemGrants: [AccountKey, 'edit' | 'view'][] = [
			['guestItemEdit', 'edit'],
			['guestPrecedence', 'view']
		];
		for (const [key, permission] of itemGrants) {
			await expectOk(
				await owner.post(`/api/v1/workspaces/${ws}/items/${grantedTask.slug}/grants`, {
					data: { user_id: accounts[key].userId, permission }
				}),
				`item grant ${key}`
			);
		}

		return {
			baseURL,
			workspaceSlug: ws,
			workspacePath: `/${accounts.owner.username}/${ws}`,
			accounts,
			tasks,
			ideas,
			grantedTask
		};
	} finally {
		await owner.dispose();
	}
}

/** Authenticate a browser context as one walk account. */
export async function actAs(context: BrowserContext, account: WalkAccount): Promise<void> {
	await context.setExtraHTTPHeaders({ Authorization: `Bearer ${account.token}` });
	await quietCrossActorToasts(context);
}
