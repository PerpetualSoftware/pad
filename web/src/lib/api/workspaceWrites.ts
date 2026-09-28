// WORKSPACE WRITES (TASK-3279). Its own module rather than part of client.ts
// so the tabs store can subscribe without importing the client: dozens of
// suites mock '$lib/api/client' with only `api`, and a module-scope
// subscription through that mock would fail every one of them.
//
// PLAN-3002 Q9: an ephemeral tab becomes durable on any write in its
// workspace. Every REST write passes through client.ts's `request`, which
// reports here, so this is the one place that sees all of them. A write is a successful
// non-GET/HEAD request under `/workspaces/{slug}`, minus the POSTs that change
// nothing a user did: the collab watermark (sent by merely opening an item),
// the copy preflight, and playbook match and run (both side-effect-free).
// Listeners get the slug as it appears in the path; one that does not name a
// workspace (`/workspaces/reorder`) matches no tab and is ignored there.
type WorkspaceWriteListener = (slug: string) => void;
const workspaceWriteListeners = new Set<WorkspaceWriteListener>();
const WORKSPACE_WRITE_PATH = /^\/workspaces\/([^/?#]+)(?:[/?#]|$)/;
const NON_WRITE_POST = /\/(?:collab-watermark|copy\/preflight|playbooks\/match|playbooks\/[^/?#]+\/run)(?:[?#]|$)/;

export function onWorkspaceWrite(listener: WorkspaceWriteListener): () => void {
	workspaceWriteListeners.add(listener);
	return () => workspaceWriteListeners.delete(listener);
}

/** Exported for its test: the slug a successful request wrote to, or null. */
export function workspaceWriteSlug(path: string, method: string | undefined): string | null {
	if (!method || method === 'GET' || method === 'HEAD') return null;
	const m = WORKSPACE_WRITE_PATH.exec(path);
	if (!m) return null;
	if (method === 'POST' && NON_WRITE_POST.test(path)) return null;
	try {
		return decodeURIComponent(m[1]);
	} catch {
		return null;
	}
}

/** Called by `request()` after a request succeeds. */
export function reportWorkspaceWrite(path: string, method: string | undefined) {
	const slug = workspaceWriteSlug(path, method);
	if (slug === null) return;
	for (const listener of workspaceWriteListeners) {
		try {
			listener(slug);
		} catch {
			// A listener's failure is not the request's.
		}
	}
}
