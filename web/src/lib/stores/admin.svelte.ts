// ---------------------------------------------------------------------------
// Admin store – shared state & utilities for the admin section
// ---------------------------------------------------------------------------

// ---- Interfaces -----------------------------------------------------------

export interface AdminUser {
	id: string;
	email: string;
	username: string;
	name: string;
	role: string;
	plan: string;
	plan_expires_at: string | null;
	plan_overrides: Record<string, number> | null;
	totp_enabled: boolean;
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
	try {
		stats = await adminFetch('/admin/stats');
	} catch (e) {
		error = e instanceof Error ? e.message : 'Failed to load';
	} finally {
		loading = false;
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
	loadStats
};
