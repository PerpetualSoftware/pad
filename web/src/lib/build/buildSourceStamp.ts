// The web bundle's source stamp (TASK-3233): which commit `vite build` ran at,
// and how many entries `git status` listed under web/ at that moment. The
// server reads it out of the embedded bundle and reports it in
// /api/v1/health next to the served bundle's own digest, so a bundle built
// from uncommitted edits (a negative-control build, BUG-3129) or left over
// from another commit is visible after it ships under a clean commit stamp.
//
// It is written into static/ at buildStart, which SvelteKit copies into the
// build output. It never fails a build: with no git (a Nix sandbox, a
// tarball) both fields are null, which reads as "unknown", never as clean.

export const BUILD_SOURCE_FILE = 'build-source.json';

export interface BuildSource {
	commit: string | null;
	dirty: number | null;
}

/** Runs git with the given args in the web/ directory and returns stdout. */
export type GitRunner = (args: string[]) => string;

export function buildSourceStamp(git: GitRunner): BuildSource {
	let commit: string | null = null;
	let dirty: number | null = null;
	try {
		commit = git(['rev-parse', 'HEAD']).trim() || null;
	} catch {
		commit = null;
	}
	if (commit !== null) {
		try {
			// `-- .` scopes to web/. Ignored paths (node_modules, build,
			// .svelte-kit, this stamp) do not count.
			const out = git(['status', '--porcelain', '--', '.']);
			dirty = out.split('\n').filter((l) => l.trim() !== '').length;
		} catch {
			dirty = null;
		}
	}
	return { commit, dirty };
}
