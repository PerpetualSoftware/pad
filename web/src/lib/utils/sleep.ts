/**
 * Resolves after `ms` milliseconds. Commits nothing, so a caller that
 * resumes after it re-checks whatever it fences on (BUG-3699: the reorder
 * read-back retries check identity after every wait).
 */
export function sleep(ms: number): Promise<void> {
	return new Promise((resolve) => setTimeout(resolve, ms));
}
