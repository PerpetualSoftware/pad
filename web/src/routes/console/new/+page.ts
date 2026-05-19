import { redirect } from '@sveltejs/kit';

// /console/new used to host a separate workspace-create page (PR #579 /
// TASK-1506). IDEA-1516 §1 consolidates the two creation surfaces: the
// modal is the single create-workspace surface, and this route just
// redirects existing bookmarks / external links back to /console with a
// query param that triggers `uiStore.openCreateWorkspace()` on mount.
//
// 307 (Temporary Redirect) preserves the request method semantics and
// signals to clients that the canonical address still belongs here in
// principle — we just chose to render via the modal instead. In SPA mode
// (adapter-static + fallback: index.html) this load function runs in the
// browser and SvelteKit handles the redirect as a client-side navigation.
export const load = () => {
	throw redirect(307, '/console?openCreate=1');
};
