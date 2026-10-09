// The directions "Add Relationship" offers (TASK-2217, audit C36).
//
// A link is stored one way, source → target, and the dialog only ever made the
// CURRENT item the source, so "TASK-8 blocks this" meant opening TASK-8 and
// linking back from there. The select also never said which way an option
// points ("Blocks" which way? "Parent" read both ways), although the item's
// relationship list already renders both directions and the CLI has
// `pad item blocked-by`. Each option now names its direction, and an inverse
// one creates the same link type from the PICKED item to this one.
//
// `parent` is stored child → parent (the source is the child), so the
// existing option means "this item is a child of the one you pick".

export interface LinkDirection {
	/** The select's value; unique per option. */
	value: string;
	/** The stored link type. */
	type: 'related' | 'blocks' | 'implements' | 'split_from' | 'supersedes' | 'parent';
	/** True when the PICKED item is the source and this item the target. */
	inverse: boolean;
	label: string;
	/** How the result reads, with `{this}` for this item and `{picked}` for the other. */
	reads: string;
}

export const LINK_DIRECTIONS: LinkDirection[] = [
	{ value: 'related', type: 'related', inverse: false, label: 'Related to', reads: '{this} is related to {picked}' },
	{ value: 'blocks', type: 'blocks', inverse: false, label: 'Blocks', reads: '{this} blocks {picked}' },
	{ value: 'blocked_by', type: 'blocks', inverse: true, label: 'Blocked by', reads: '{picked} blocks {this}' },
	{ value: 'implements', type: 'implements', inverse: false, label: 'Implements', reads: '{this} implements {picked}' },
	{ value: 'implemented_by', type: 'implements', inverse: true, label: 'Implemented by', reads: '{picked} implements {this}' },
	{ value: 'child_of', type: 'parent', inverse: false, label: 'Child of', reads: '{picked} becomes the parent of {this}' },
	{ value: 'parent_of', type: 'parent', inverse: true, label: 'Parent of', reads: '{picked} becomes a child of {this}' },
	{ value: 'split_from', type: 'split_from', inverse: false, label: 'Split from', reads: '{this} was split from {picked}' },
	{ value: 'split_into', type: 'split_from', inverse: true, label: 'Split into', reads: '{picked} was split from {this}' },
	{ value: 'supersedes', type: 'supersedes', inverse: false, label: 'Supersedes', reads: '{this} supersedes {picked}' },
	{ value: 'superseded_by', type: 'supersedes', inverse: true, label: 'Superseded by', reads: '{picked} supersedes {this}' }
];

export function linkDirection(value: string): LinkDirection {
	return LINK_DIRECTIONS.find((d) => d.value === value) ?? LINK_DIRECTIONS[0];
}

/** The sentence under the select, e.g. "TASK-5 blocks the item you pick". */
export function linkSentence(d: LinkDirection, thisRef: string): string {
	return d.reads.replace('{this}', thisRef).replace('{picked}', 'the item you pick');
}

/**
 * The create call's addressing: which item the link is created FROM (the
 * route's item) and which id it targets.
 */
export function linkEnds(
	d: LinkDirection,
	current: { slug: string; id: string },
	picked: { slug: string; id: string }
): { fromSlug: string; targetId: string } {
	return d.inverse ? { fromSlug: picked.slug, targetId: current.id } : { fromSlug: current.slug, targetId: picked.id };
}
