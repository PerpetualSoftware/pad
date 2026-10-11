// "Back to what you were doing" after an upgrade (TASK-3601). Checkout leaves
// and returns in the same tab and origin (the sidecar's success_url is
// /console/billing?checkout=success), so the blocked flow is remembered in
// sessionStorage before leaving and offered on return. Short-lived and
// one-shot: an hour, cleared once used or dismissed. The action is offered,
// never re-run on the user's behalf.

const KEY = 'pad:resume-after-upgrade';
export const UPGRADE_RESUME_TTL_MS = 60 * 60 * 1000;

export interface UpgradeResume {
	/** Where the blocked action lives: a same-origin path with its query. */
	path: string;
	/** What was blocked, as a short phrase: "invite a member". */
	action: string;
	at: number;
}

function storage(): Storage | null {
	try {
		return globalThis.sessionStorage ?? null;
	} catch {
		return null;
	}
}

function safePath(path: string): boolean {
	return path.startsWith('/') && !path.startsWith('//') && !path.startsWith('/\\');
}

export function saveUpgradeResume(path: string, action: string, now = Date.now()): void {
	if (!safePath(path)) return;
	storage()?.setItem(KEY, JSON.stringify({ path, action, at: now } satisfies UpgradeResume));
}

export function readUpgradeResume(now = Date.now()): UpgradeResume | null {
	const s = storage();
	const raw = s?.getItem(KEY);
	if (!raw) return null;
	try {
		const v = JSON.parse(raw) as UpgradeResume;
		if (typeof v.path !== 'string' || !safePath(v.path) || typeof v.at !== 'number' || now - v.at > UPGRADE_RESUME_TTL_MS) {
			s?.removeItem(KEY);
			return null;
		}
		return { path: v.path, action: typeof v.action === 'string' ? v.action : '', at: v.at };
	} catch {
		s?.removeItem(KEY);
		return null;
	}
}

export function clearUpgradeResume(): void {
	storage()?.removeItem(KEY);
}
