// Where the ACTIVE workspace tab lands when that tab goes away (PLAN-3002
// Q3): its left neighbour, or, when it was the first tab, the tab that
// becomes first (its right neighbour; lead ruling on TASK-3274). `null`
// means no tab is left, which lands on /console. The caller turns a tab into
// a URL at that tab's last route (workspaceRestoreTarget) and decides
// whether a navigation is owed at all: only losing the ACTIVE tab moves you.
//
// `before` is the open set in bar order BEFORE the tab went away; `after` is
// the open set now. They are separate because `after` is not always
// `before` minus one: a refetch after a lost workspace (TASK-3275) can also
// drop or add other tabs, so the neighbour is chosen from `before` and must
// still be in `after`, else the landing is `after`'s first tab.
export function tabLanding<T extends { slug: string }>(
	before: readonly { slug: string }[],
	goneSlug: string,
	after: readonly T[]
): T | null {
	if (after.length === 0) return null;
	const idx = before.findIndex((t) => t.slug === goneSlug);
	const neighbour = idx > 0 ? before[idx - 1]?.slug : before[idx + 1]?.slug;
	return after.find((t) => t.slug === neighbour) ?? after[0];
}
