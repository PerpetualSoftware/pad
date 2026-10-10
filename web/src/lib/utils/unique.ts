// Helpers that make a list safe to render in a keyed {#each} (TASK-3539).
//
// Svelte 5 throws each_key_duplicate during render, in production builds too,
// when two entries of a keyed each produce the same key, so one repeated
// entry blanks the whole component (BUG-3538). A list whose source cannot
// promise distinct keys goes through one of these before it renders.

/** The entries of `list` whose key is seen for the first time, in order. */
export function uniqueBy<T, K>(list: readonly T[], key: (entry: T) => K): T[] {
	const seen = new Set<K>();
	const out: T[] = [];
	for (const entry of list) {
		const k = key(entry);
		if (seen.has(k)) continue;
		seen.add(k);
		out.push(entry);
	}
	return out;
}

/** The distinct strings of `list`, in first-seen order. */
export function uniqueStrings(list: readonly string[]): string[] {
	return uniqueBy(list, (s) => s);
}
