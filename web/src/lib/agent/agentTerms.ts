// What the agent-facing metadata means, in words (TASK-2257, audit C72).
//
// Conventions, playbooks and the library showed trigger, surface/scope and
// enforcement as bare tokens ("always", "all", "must") and enforcement as an
// 8px coloured dot with a title-only label: invisible on touch, silent to a
// screen reader, and unexplained to a teammate who did not write the rule.
// One vocabulary for the three pages: the legend, the chips' tooltips and the
// visually hidden prefixes all read from here.

export const TERMS = {
	trigger: 'When an agent loads it: "always" means every session; others name a moment, such as on-commit or on-implement.',
	surface: 'Which agents or tools it applies to: "all" means every one.',
	enforcement:
		'How binding it is: "must" is required, "should" is expected unless there is a reason not to, "nice-to-have" is a preference.'
} as const;

export function termTitle(term: keyof typeof TERMS, value: string): string {
	const name = term === 'surface' ? 'Surface' : term === 'trigger' ? 'Trigger' : 'Enforcement';
	return `${name}: ${value}. ${TERMS[term]}`;
}

/** The visible label for an enforcement value ("nice-to-have" reads "nice to have"). */
export function enforcementLabel(value: string): string {
	return value.replace(/-/g, ' ');
}
