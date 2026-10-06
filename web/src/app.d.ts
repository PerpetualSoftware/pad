// See https://svelte.dev/docs/kit/types#app.d.ts
// for information about these interfaces
declare global {
	namespace App {
		// interface Error {}
		// interface Locals {}
		// interface PageData {}
		// Split-pane mini-browser depth/ownership stamp (PLAN-2154 /
		// TASK-2157). Carried in SvelteKit `page.state` (NOT raw
		// `history.state`) so it follows opaque Back/Forward, survives
		// `history.go`, and reconstructs on cold-load. See
		// `$lib/collections/paneController.ts`.
		interface PageState {
			paneDepth?: number;
			paneOwned?: boolean;
		}
		// interface Platform {}
	}
}

export {};
