/**
 * Shared demo data — single source of truth for "what a real-feeling Pad
 * workspace looks like."
 *
 * Two consumers today:
 *   - web/e2e/lib/demo-seed.ts        (Playwright fixture seeder; turns
 *                                      this data into HTTP POSTs against a
 *                                      live pad server)
 *   - pad-remotion/src/data/demoItems.ts (Remotion compositions; uses
 *                                      this data to populate kanban cards
 *                                      and context-stream ghosts)
 *
 * pad-remotion lives in a sibling repo, so it carries a copy of this file
 * rather than a path import. Keep the shapes and titles identical — when
 * you change anything here, mirror the change in pad-remotion. A CI drift
 * check is a possible follow-up if this duplication starts to bite.
 */

export interface DemoPlan {
	/** Plan title rendered in cards / scenes. */
	title: string;
	/** Pad plan status (`active`, `planned`, `completed`, `paused`). */
	status: string;
	/** Markdown body — short, marketing-friendly. */
	content: string;
}

export interface DemoTask {
	title: string;
	/** Pad task status (`open`, `in-progress`, `done`, `cancelled`). */
	status: string;
	/** Pad task priority (`low`, `medium`, `high`, `critical`). */
	priority: string;
	/** Pad effort estimate (`xs`, `s`, `m`, `l`, `xl`). */
	effort?: string;
	/**
	 * Whether this task is parented to `demoPlan`. The seeder maps this to
	 * the freshly-created plan id; pad-remotion uses it to render
	 * parent-child link visuals.
	 */
	parentToPlan?: boolean;
}

export interface DemoIdea {
	title: string;
	/** Pad idea status (`new`, `exploring`, `planned`, `implemented`, `rejected`). */
	status: string;
	/** Pad idea impact (`low`, `medium`, `high`). */
	impact: string;
}

export interface DemoConvention {
	title: string;
	/** Pad convention trigger (`always`, `on-implement`, `on-commit`, ...). */
	trigger: string;
	priority: 'must' | 'should' | 'nice-to-have';
	/** Defaults to `all` when omitted. */
	scope?: 'all' | 'backend' | 'frontend' | 'mobile' | 'docs' | 'devops';
}

/**
 * Active plan with non-zero progress for the dashboard "Active Plans" widget.
 */
export const demoPlan: DemoPlan = {
	title: 'v0.2 — Collaboration',
	status: 'active',
	content: 'Real-time multiplayer editing, team invites, and shared views.'
};

/**
 * Realistic mix of statuses, priorities, parent linkage. Designed so the
 * board view shows meaningful columns and the dashboard's plan progress
 * widget reads non-zero (3 of 5 plan-linked tasks are in-flight or done).
 */
export const demoTasks: DemoTask[] = [
	{
		title: 'Fix OAuth redirect bug',
		status: 'in-progress',
		priority: 'high',
		effort: 'm',
		parentToPlan: true
	},
	{
		title: 'Add rate limiting to API endpoints',
		status: 'in-progress',
		priority: 'high',
		effort: 'l',
		parentToPlan: true
	},
	{
		title: 'Refactor auth middleware',
		status: 'open',
		priority: 'medium',
		effort: 'm'
	},
	{
		title: 'Add SSO support for enterprise users',
		status: 'open',
		priority: 'medium',
		effort: 'xl',
		parentToPlan: true
	},
	{
		title: 'Document the deploy pipeline',
		status: 'open',
		priority: 'low',
		effort: 's'
	},
	{
		title: 'Update Go to 1.26',
		status: 'done',
		priority: 'medium',
		effort: 's'
	},
	{
		title: 'Set up CI security scanning',
		status: 'done',
		priority: 'high',
		effort: 's',
		parentToPlan: true
	}
];

/**
 * Two ideas at different stages — gives the ideas page something to render.
 */
export const demoIdeas: DemoIdea[] = [
	{ title: 'Real-time presence indicators', status: 'exploring', impact: 'high' },
	{ title: 'Slack integration for notifications', status: 'new', impact: 'medium' }
];

/**
 * A handful of conventions covering the common triggers. Used by
 * pad-remotion's ContextScene to populate ghost-card streams; not
 * currently consumed by demo-seed.ts (which calls `seedConventions`
 * directly with caller-supplied input), but lives here so the shared
 * data shape is complete.
 */
export const demoConventions: DemoConvention[] = [
	{
		title: 'Run tests before pushing',
		trigger: 'on-commit',
		priority: 'must',
		scope: 'all'
	},
	{
		title: 'Use conventional commit format',
		trigger: 'on-commit',
		priority: 'should',
		scope: 'all'
	},
	{
		title: 'Tasks should be PR-sized',
		trigger: 'on-plan',
		priority: 'should',
		scope: 'all'
	},
	{
		title: 'Codex review every PR before merge',
		trigger: 'on-pr-create',
		priority: 'must',
		scope: 'all'
	}
];
