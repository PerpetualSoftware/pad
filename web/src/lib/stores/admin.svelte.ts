// ---------------------------------------------------------------------------
// Admin store – shared state & utilities for the admin section
// ---------------------------------------------------------------------------

import { authStore } from './auth.svelte';

// ---- Interfaces -----------------------------------------------------------

export interface AdminUser {
	id: string;
	email: string;
	username: string;
	name: string;
	role: string;
	plan: string;
	plan_expires_at: string | null;
	/**
	 * Per-user limit overrides as a raw JSON string (e.g.
	 * `{"storage_bytes":10737418240}`). The API returns the raw
	 * column value rather than a decoded object, so the UI must
	 * JSON.parse it before reading keys. Empty string = no overrides.
	 */
	plan_overrides: string | null;
	totp_enabled: boolean;
	disabled_at: string | null;
	/**
	 * Email-verification timestamp (RFC3339) or null/undefined when the
	 * user hasn't confirmed their email. Mirrors disabled_at. Surfaced by
	 * the admin list + get-user handlers (PLAN-1933 Wave 1's SearchUsers
	 * scan). Drives the "Mark email verified" force-verify override in the
	 * admin user panel — shown only while this is falsy. TASK-1939 / DR-7.
	 */
	email_verified_at: string | null;
	last_active_at: string | null;
	/**
	 * Last mutating action (item/comment/attachment) — distinct from
	 * last_active_at, which fires on any authenticated request including
	 * reads. Wired by Store.TouchUserWrite. PLAN-1542 / TASK-1543.
	 */
	last_write_at: string | null;
	/** Number of non-deleted workspaces this user owns. */
	workspace_count: number;
	/**
	 * Total attachment bytes across owned workspaces (matches
	 * WorkspaceStorageUsage's definition; includes derived blobs).
	 */
	storage_bytes: number;
	/**
	 * Computed status pill. Precedence: disabled > no-workspace > inactive
	 * (>30d or never wrote) > active. Server-side in computeAdminUserStatus.
	 */
	status: 'active' | 'inactive' | 'disabled' | 'no-workspace';
	created_at: string;
}

export interface Stats {
	users: number;
	users_by_plan: Record<string, number>;
	workspaces: number;
	cloud_mode: boolean;
}

export interface LimitTiers {
	free: Record<string, number>;
	pro: Record<string, number>;
}

// ---- Helper functions -----------------------------------------------------

export function getCSRFToken(): string | null {
	const hostMatch = document.cookie.match(/(?:^|;\s*)__Host-pad_csrf=([^;]+)/);
	if (hostMatch) return hostMatch[1];
	const match = document.cookie.match(/(?:^|;\s*)pad_csrf=([^;]+)/);
	return match ? match[1] : null;
}

export async function adminFetch(path: string, opts?: RequestInit) {
	const resp = await fetch('/api/v1' + path, { credentials: 'same-origin', ...opts });
	if (!resp.ok) throw new Error(`${resp.status}`);
	return resp.json();
}

export async function adminPatch(path: string, body: unknown) {
	const headers: Record<string, string> = { 'Content-Type': 'application/json' };
	const csrf = getCSRFToken();
	if (csrf) headers['X-CSRF-Token'] = csrf;
	return adminFetch(path, {
		method: 'PATCH',
		headers,
		body: JSON.stringify(body)
	});
}

export async function adminPost(path: string, body?: unknown) {
	const headers: Record<string, string> = { 'Content-Type': 'application/json' };
	const csrf = getCSRFToken();
	if (csrf) headers['X-CSRF-Token'] = csrf;
	const opts: RequestInit = { method: 'POST', headers };
	if (body !== undefined) opts.body = JSON.stringify(body);
	return adminFetch(path, opts);
}

export function formatDate(d: string): string {
	return new Date(d).toLocaleDateString('en-US', {
		month: 'short',
		day: 'numeric',
		year: 'numeric'
	});
}

// ---- Reactive store -------------------------------------------------------

let stats = $state<Stats | null>(null);
let loading = $state(true);
let error = $state('');

async function loadStats() {
	loading = true;
	error = '';
	// The identity that ASKED (BUG-3005, codex round 2). Clearing on the signal
	// is not enough on its own: a request already in flight settles afterwards
	// and writes the previous admin's statistics into the new session.
	const isSameIdentity = authStore.identityFence();
	try {
		const result = await adminFetch('/admin/stats');
		if (!isSameIdentity()) return;
		stats = result;
	} catch (e) {
		if (!isSameIdentity()) return;
		error = e instanceof Error ? e.message : 'Failed to load';
	} finally {
		if (isSameIdentity()) loading = false;
	}
}

export const adminStore = {
	get stats() {
		return stats;
	},
	get loading() {
		return loading;
	},
	get error() {
		return error;
	},
	loadStats,

	/**
	 * Drop the loaded statistics (BUG-3005). `stats` is instance-wide DATA but
	 * it is authorization-scoped: only an admin can read it, so it must not
	 * outlive the admin who did.
	 */
	clear() {
		stats = null;
		// FALSE, not true (codex round 2). The admin layout loads on MOUNT, and
		// a same-route admin-to-admin swap mounts nothing — leaving this true
		// would pin the console on "Loading admin data…" forever. The layout
		// re-issues the load on the same signal; until it lands the honest
		// state is "nothing loaded", not "loading".
		loading = false;
		error = '';
	}
};

// An admin signing out — or a swap to a non-admin on the same console route —
// must not leave the previous admin's statistics on screen. The admin layout
// loads on mount, and a same-route identity change mounts nothing (BUG-3005,
// codex round 1).
authStore.onIdentityChange(() => {
	adminStore.clear();
});
