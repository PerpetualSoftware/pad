import type {
	AppInstallList,
	AppInstallPreview,
	AppInstallStateResult,
	AppUpgradeConfirmResult
} from '$lib/types';

/**
 * Every list the apps UI reads, made an array (BUG-3417). A Go server
 * serializes an omitted slice as null, so a minimal manifest's preview came
 * back with item_actions, artifacts, events and collections as null, and the
 * review's `.length` threw. The server now sends []; these keep the UI whole
 * against an older server or a list a later field adds before its server
 * fix. Applied where each response lands, so no component reads a raw one.
 */
const arr = <T>(v: T[] | null | undefined): T[] => (Array.isArray(v) ? v : []);

export function normalizePreview(p: AppInstallPreview): AppInstallPreview {
	return {
		...p,
		collections: arr(p.collections),
		events: arr(p.events).map((e) => ({ ...e, collections: arr(e.collections) })),
		item_actions: arr(p.item_actions).map((a) => ({ ...a, collections: arr(a.collections) })),
		artifacts: arr(p.artifacts).map((a) => ({
			...a,
			changes: arr(a.changes),
			normalized: { ...a.normalized, fields: a.normalized?.fields ?? {} }
		})),
		redirect_uris: arr(p.redirect_uris),
		upgrade: p.upgrade ? { ...p.upgrade, diff: arr(p.upgrade.diff) } : p.upgrade
	};
}

export function normalizeInstallList(l: AppInstallList): AppInstallList {
	return { ...l, installs: arr(l.installs) };
}

export function normalizeInstallState(s: AppInstallStateResult): AppInstallStateResult {
	return s.artifacts === undefined ? s : { ...s, artifacts: arr(s.artifacts) };
}

export function normalizeUpgradeResult(r: AppUpgradeConfirmResult): AppUpgradeConfirmResult {
	return { ...r, items: arr(r.items) };
}
