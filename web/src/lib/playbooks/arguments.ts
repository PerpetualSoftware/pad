/**
 * Playbook argument spec — the structured form of the body's `## Arguments`
 * section. Mirrors the server-side `PlaybookArgumentSpec` shape
 * (`internal/server/handlers_playbooks.go`).
 *
 * Five types per PLAN-1377's design:
 *   - ref     issue ID (TASK-5) or item slug
 *   - string  free-form text
 *   - flag    presence-or-absent boolean (no value)
 *   - enum    one of a fixed list (carries `enum` options)
 *   - number  finite float
 *
 * `default` is opaque — agents handle non-literal defaults like
 * "current git branch" themselves; the editor stores whatever the
 * user typed.
 */
export type PlaybookArgumentType = 'ref' | 'string' | 'flag' | 'enum' | 'number';

export interface PlaybookArgument {
	name: string;
	type: PlaybookArgumentType;
	required?: boolean;
	default?: string | boolean | number;
	description?: string;
	enum?: string[];
}

export const PLAYBOOK_ARGUMENT_TYPES: readonly PlaybookArgumentType[] = [
	'string',
	'ref',
	'flag',
	'enum',
	'number'
] as const;

/**
 * INVOCATION_SLUG_PATTERN mirrors the server-side regex
 * (`collections.PlaybookInvocationSlugPattern`):
 *   - 2+ chars (single-letter slugs would shadow NL tokens)
 *   - lowercase letters, digits, hyphens
 *   - no leading/trailing hyphen
 *
 * Kept lockstep with `internal/collections/templates.go::PlaybookInvocationSlugPattern`.
 * Drift between the two means the client allows slugs the server rejects
 * (or vice-versa), which surfaces as a confusing save error.
 */
export const INVOCATION_SLUG_PATTERN = /^[a-z0-9][a-z0-9-]*[a-z0-9]$/;

/** Returns true if the slug matches the canonical kebab-case pattern. */
export function isValidInvocationSlug(slug: string): boolean {
	return INVOCATION_SLUG_PATTERN.test(slug);
}

/** Skeleton body inserted when a new playbook is created. The four
 * sections match the PLAN-1377 design vocabulary; the user can delete
 * sections that don't apply. */
export const PLAYBOOK_SKELETON_BODY = `One-paragraph summary of what this playbook does and when to run it.

## Arguments

- \`example\` (string, required) — describe what this argument is for

## Steps

1. First step — describe it
2. Second step — describe it
3. Third step — describe it

## Defaults

Notes on default behavior, environment expectations, or assumed context.

## Stop conditions

When to stop running the playbook and ask the user (errors that need
human judgment, ambiguous state, missing requirements).
`;

const ARGUMENTS_HEADING = /^##\s+Arguments\s*$/m;

/**
 * splitAroundArguments finds the `## Arguments` section in `body`
 * (case-insensitive on the heading) and returns the three pieces around
 * it: text before the heading, the section's body (between the heading
 * and the next `## ` heading), and the text from that next heading
 * onward.
 *
 * Returns null when the body has no `## Arguments` heading at all.
 * The caller decides whether to insert one or skip the update.
 */
export function splitAroundArguments(body: string): {
	before: string;
	section: string;
	after: string;
} | null {
	const match = ARGUMENTS_HEADING.exec(body);
	if (!match) return null;
	const headingStart = match.index;
	const headingEnd = headingStart + match[0].length;
	// Find the next `## ` heading after the Arguments heading. Allow a
	// trailing newline before it. The regex is anchored at line start.
	const tail = body.slice(headingEnd);
	const nextHeadingRe = /\n##\s+\S/;
	const nextMatch = nextHeadingRe.exec(tail);
	const sectionEnd = nextMatch ? headingEnd + nextMatch.index : body.length;
	return {
		before: body.slice(0, headingStart),
		// Include the heading line itself so the replacement is canonical.
		section: body.slice(headingStart, sectionEnd),
		after: body.slice(sectionEnd)
	};
}

/**
 * formatArgumentLine produces the canonical one-line markdown rendering
 * of a single PlaybookArgument. The grammar is what parseArgumentLine
 * accepts (round-trip safe).
 *
 * Examples:
 *   - `target` (string, required) — what to ship
 *   - `stop-after-each` (flag, default=false) — pause for confirmation
 *   - `merge-strategy` (enum, default=squash, options: squash|merge|rebase) — how
 */
export function formatArgumentLine(arg: PlaybookArgument): string {
	const bits: string[] = [arg.type];
	if (arg.required) bits.push('required');
	if (arg.default !== undefined && arg.default !== '' && arg.default !== null) {
		bits.push(`default=${stringifyDefault(arg.default)}`);
	}
	if (arg.type === 'enum' && arg.enum && arg.enum.length > 0) {
		bits.push(`options: ${arg.enum.join('|')}`);
	}
	const inner = bits.join(', ');
	const desc = arg.description ? ` — ${arg.description}` : '';
	return `- \`${arg.name}\` (${inner})${desc}`;
}

function stringifyDefault(v: string | boolean | number): string {
	if (typeof v === 'boolean') return v ? 'true' : 'false';
	if (typeof v === 'number') return String(v);
	return v;
}

// The line an empty arguments section shows; the only line in the section
// updateArgumentsInBody owns outright.
const NO_ARGUMENTS_PLACEHOLDER = '(No arguments — this playbook takes no inputs.)';

/**
 * renderArgumentsSection produces the full `## Arguments` section as a
 * string (heading + blank line + one bullet per argument + trailing
 * blank line). Empty arg list returns just the heading + a placeholder
 * note so the section stays discoverable when the user deletes all args.
 */
export function renderArgumentsSection(args: PlaybookArgument[]): string {
	if (args.length === 0) {
		return `## Arguments\n\n${NO_ARGUMENTS_PLACEHOLDER}\n`;
	}
	const lines = args.map(formatArgumentLine);
	return `## Arguments\n\n${lines.join('\n')}\n`;
}

/**
 * updateArgumentsInBody writes `args` into the body's `## Arguments`
 * section, preserving everything before and after it. If the body has no
 * such heading, the rendered section is appended (with a leading blank line
 * so it doesn't collide with the prior content).
 *
 * TASK-2191: the section is edited IN PLACE, not regenerated. It used to be
 * replaced wholesale with bullets on every keystroke in the arguments form,
 * which deleted any prose an author wrote there and rewrote every bullet in
 * canonical form, dropping the tokens parseArgumentLine tolerates. Now:
 * - every line that is not an argument bullet (prose, blank lines, a
 *   sub-heading) stays where it is;
 * - the existing bullet lines are slots, filled in the form's order; an
 *   argument the form left unchanged keeps its ORIGINAL line verbatim, and a
 *   changed one is re-rendered in its slot;
 * - an argument with no slot left goes after the last bullet; a slot with no
 *   argument left is removed;
 * - the "no arguments" placeholder is the one line this owns: it is dropped
 *   when there are arguments, and added when there are none and nothing
 *   else is in the section.
 *
 * An unchanged argument is found by name first, then by position among
 * those left, so a rename re-renders its own bullet rather than moving.
 */
export function updateArgumentsInBody(body: string, args: PlaybookArgument[]): string {
	// A body saved with CRLF (codex r1): a bullet's trailing \r hid it from the
	// parser, so its slot was missed and an edit left the old bullet beside
	// the new one. Edit it as LF and give the CRLF back.
	if (body.includes('\r\n')) {
		return updateArgumentsInBody(body.replace(/\r\n/g, '\n'), args).replace(/\n/g, '\r\n');
	}
	const split = splitAroundArguments(body);
	if (!split) {
		const newSection = renderArgumentsSection(args).replace(/\n+$/, '\n');
		const trimmed = body.replace(/\n+$/, '');
		const prefix = trimmed.length > 0 ? `${trimmed}\n\n` : '';
		return `${prefix}${newSection}`;
	}

	const [heading, ...rest] = split.section.split('\n');
	const lines = rest.filter((l) => l.trim() !== NO_ARGUMENTS_PLACEHOLDER);
	const slots: number[] = [];
	const existing: (PlaybookArgument | null)[] = lines.map((l) => parseArgumentLine(l));
	existing.forEach((a, i) => {
		if (a) slots.push(i);
	});

	// Pair each new argument with the old bullet it came from: by name, then
	// by position among the bullets nobody claimed by name.
	const claimed = new Set<number>();
	const source: (number | null)[] = args.map((a) => {
		const at = slots.find((i) => !claimed.has(i) && existing[i]!.name === a.name);
		if (at === undefined) return null;
		claimed.add(at);
		return at;
	});
	source.forEach((at, k) => {
		if (at !== null) return;
		const free = slots.find((i) => !claimed.has(i));
		if (free !== undefined) {
			claimed.add(free);
			source[k] = free;
		}
	});

	const rendered = args.map((a, k) => {
		const at = source[k];
		if (at !== null && formatArgumentLine(existing[at]!) === formatArgumentLine(a)) return lines[at];
		return formatArgumentLine(a);
	});

	// Fill the slots in order; extra arguments go after the last slot.
	const out: string[] = [];
	let next = 0;
	const lastSlot = slots.length > 0 ? slots[slots.length - 1] : -1;
	lines.forEach((line, i) => {
		if (existing[i]) {
			if (next < rendered.length) out.push(rendered[next++]);
		} else {
			out.push(line);
		}
		if (i === lastSlot) while (next < rendered.length) out.push(rendered[next++]);
	});
	if (next < rendered.length) {
		// No bullet to follow: after the heading's blank line, before any
		// trailing blank lines.
		let at = out.length;
		while (at > 0 && out[at - 1].trim() === '') at--;
		const insert = rendered.slice(next);
		if (at === 0) out.splice(0, 0, '', ...insert);
		else out.splice(at, 0, ...insert);
	}

	const hasContent = out.some((l) => l.trim() !== '');
	if (args.length === 0 && !hasContent) {
		out.splice(0, out.length, '', NO_ARGUMENTS_PLACEHOLDER);
	}

	const section = [heading, ...out].join('\n').replace(/\n+$/, '') + '\n';
	const after = split.after.replace(/^\n+/, '');
	const before = split.before.replace(/\n+$/, '');
	const pieces: string[] = [];
	if (before) pieces.push(before, '\n\n');
	pieces.push(section);
	if (after) pieces.push('\n', after);
	return pieces.join('');
}

const BULLET_RE = /^\s*-\s*`([^`]+)`\s*(?:\(([^)]*)\))?\s*(?:[—–-]\s*(.*))?$/;

/**
 * parseArgumentLine extracts a single PlaybookArgument from a bullet
 * line in canonical or near-canonical form. Returns null if the line
 * doesn't look like an argument bullet (so the caller can ignore prose
 * lines mixed into the section).
 *
 * Recognized parenthetical tokens:
 *   - type (first bare word matching PLAYBOOK_ARGUMENT_TYPES)
 *   - "required"
 *   - "default=<value>"
 *   - "options: a|b|c" or "enum: a|b|c" (for enum types)
 *
 * Unknown tokens are ignored — the parser is permissive on input so
 * hand-written sections don't lose data on a round-trip-then-re-render.
 */
export function parseArgumentLine(line: string): PlaybookArgument | null {
	const m = BULLET_RE.exec(line);
	if (!m) return null;
	const name = m[1].trim();
	const inner = (m[2] || '').trim();
	const description = (m[3] || '').trim();
	if (!name) return null;

	const arg: PlaybookArgument = { name, type: 'string' };
	if (description) arg.description = description;
	if (!inner) return arg;

	// Split on commas that are NOT inside `options: a|b|c` — but we
	// treat each segment independently and look for `options:` /
	// `enum:` prefixes anywhere. A two-pass split is unnecessary here
	// because pipes can't legitimately appear in other tokens.
	const segments = inner.split(',').map((s) => s.trim()).filter(Boolean);
	for (const seg of segments) {
		const lower = seg.toLowerCase();
		if ((PLAYBOOK_ARGUMENT_TYPES as readonly string[]).includes(lower)) {
			arg.type = lower as PlaybookArgumentType;
			continue;
		}
		if (lower === 'required') {
			arg.required = true;
			continue;
		}
		if (lower === 'optional') {
			// Explicit optional marker — already the default, but tolerate
			// it in hand-written sections.
			arg.required = false;
			continue;
		}
		const defMatch = /^default\s*=\s*(.+)$/i.exec(seg);
		if (defMatch) {
			arg.default = coerceDefaultValue(defMatch[1].trim());
			continue;
		}
		const optMatch = /^(?:options|enum)\s*[:=]\s*(.+)$/i.exec(seg);
		if (optMatch) {
			arg.enum = optMatch[1]
				.split('|')
				.map((s) => s.trim())
				.filter(Boolean);
			continue;
		}
		// Unknown segment — drop it. The canonical formatter will
		// re-emit only well-formed tokens on the next round-trip.
	}
	return arg;
}

function coerceDefaultValue(raw: string): string | boolean | number {
	if (raw === 'true') return true;
	if (raw === 'false') return false;
	if (/^-?\d+(?:\.\d+)?$/.test(raw)) {
		const n = Number(raw);
		if (Number.isFinite(n)) return n;
	}
	return raw;
}

/**
 * parseArgumentsSection reads the `## Arguments` section out of the
 * playbook body and returns the structured arg list. Lines that aren't
 * argument bullets are silently dropped (the section can carry intro
 * prose).
 */
export function parseArgumentsSection(body: string): PlaybookArgument[] {
	const split = splitAroundArguments(body);
	if (!split) return [];
	// Strip the heading line itself.
	const lines = split.section.split(/\r?\n/);
	const out: PlaybookArgument[] = [];
	for (const line of lines) {
		const arg = parseArgumentLine(line);
		if (arg) out.push(arg);
	}
	return out;
}

/**
 * argumentsToJSON serializes the structured arg list into the JSON
 * shape stored in the playbook item's `arguments` field. Returns "[]"
 * for an empty list so the field is always valid JSON.
 *
 * Empty-string defaults are dropped — they're indistinguishable from
 * "no default" and the server treats them the same.
 *
 * Defaults are coerced to the type the arg declares before
 * serialization (Codex round 4 P2). The UI input always types into a
 * string, but `flag` defaults should be booleans and `number` defaults
 * should be numbers — the server passes them through opaquely so the
 * client is responsible for the canonical typing.
 */
export function argumentsToJSON(args: PlaybookArgument[]): string {
	const cleaned = args.map((arg) => {
		const out: Record<string, unknown> = { name: arg.name, type: arg.type };
		if (arg.required) out.required = true;
		if (arg.default !== undefined && arg.default !== '' && arg.default !== null) {
			out.default = coerceDefaultForType(arg.type, arg.default);
		}
		if (arg.description) out.description = arg.description;
		if (arg.enum && arg.enum.length > 0) out.enum = arg.enum;
		return out;
	});
	return JSON.stringify(cleaned);
}

/**
 * coerceDefaultForType applies type-aware coercion to a default value
 * collected from the UI. Mirrors the parsing rules in
 * `coerceDefaultValue` (used by parseArgumentLine) so round-trips
 * between the markdown body, the structured form, and the persisted
 * JSON stay consistent.
 *
 * - flag: "true"/"false" → boolean; anything else falls back to the
 *   original value (the schema validator will surface it)
 * - number: numeric strings → number; non-numeric falls through
 * - other types: pass through unchanged
 */
function coerceDefaultForType(
	type: PlaybookArgumentType,
	raw: string | boolean | number
): string | boolean | number {
	if (type === 'flag') {
		if (typeof raw === 'boolean') return raw;
		if (typeof raw === 'string') {
			const lower = raw.trim().toLowerCase();
			if (lower === 'true') return true;
			if (lower === 'false') return false;
		}
		return raw;
	}
	if (type === 'number') {
		if (typeof raw === 'number') return raw;
		if (typeof raw === 'string') {
			const trimmed = raw.trim();
			if (/^-?\d+(?:\.\d+)?$/.test(trimmed)) {
				const n = Number(trimmed);
				if (Number.isFinite(n)) return n;
			}
		}
		return raw;
	}
	return raw;
}

/** argumentsFromJSON parses the stored `arguments` field back into the
 * structured form. Returns [] on any decode failure so the form
 * gracefully falls back to "no args declared" rather than crashing on
 * malformed legacy data. */
export function argumentsFromJSON(raw: unknown): PlaybookArgument[] {
	if (raw == null) return [];
	let parsed: unknown = raw;
	if (typeof raw === 'string') {
		if (raw.trim() === '') return [];
		try {
			parsed = JSON.parse(raw);
		} catch {
			return [];
		}
	}
	if (!Array.isArray(parsed)) return [];
	return parsed
		.map((entry): PlaybookArgument | null => {
			if (!entry || typeof entry !== 'object') return null;
			const e = entry as Record<string, unknown>;
			const name = typeof e.name === 'string' ? e.name : '';
			if (!name) return null;
			const type = typeof e.type === 'string' ? (e.type as PlaybookArgumentType) : 'string';
			const arg: PlaybookArgument = { name, type };
			if (e.required === true) arg.required = true;
			if (e.default !== undefined && e.default !== null) {
				arg.default = e.default as PlaybookArgument['default'];
			}
			if (typeof e.description === 'string' && e.description) arg.description = e.description;
			if (Array.isArray(e.enum)) {
				arg.enum = (e.enum as unknown[]).filter((v): v is string => typeof v === 'string');
			}
			return arg;
		})
		.filter((a): a is PlaybookArgument => a !== null);
}

/**
 * buildTestInvocation renders the example commands for the
 * "Test invocation" helper. Returns three forms so power users can
 * see each surface side by side:
 *   - claude: `/pad <slug> <tokens>` (agent NL form)
 *   - cli:    `pad playbook run <slug> [tokens]` (strict positional form)
 *   - mcp:    `pad_playbook { action: "run", ref: "<slug>", args: {...} }`
 *
 * `values` is keyed by argument name and carries the user-supplied
 * sample input for each spec entry. Empty/unset values are skipped so
 * the rendered command doesn't show empty=`= ` pairs.
 */
export interface TestInvocationRendering {
	/**
	 * The canonical, surface-neutral form: plain natural language. Works on
	 * every agent surface (PLAN-1858 / IDEA-1846). The other fields are
	 * per-surface shortcuts that resolve to the same playbook.
	 */
	nl: string;
	claude: string;
	cli: string;
	mcp: string;
}

export function buildTestInvocation(
	slug: string,
	args: PlaybookArgument[],
	values: Record<string, string>
): TestInvocationRendering {
	const safeSlug = slug || '<slug>';
	const claudeTokens: string[] = [];
	const cliTokens: string[] = [];
	const mcpArgs: Record<string, unknown> = {};

	for (const arg of args) {
		const raw = (values[arg.name] ?? '').trim();
		if (arg.type === 'flag') {
			if (raw === 'true' || raw === '1' || raw === 'yes') {
				claudeTokens.push(arg.name);
				cliTokens.push(arg.name);
				mcpArgs[arg.name] = true;
			}
			continue;
		}
		if (!raw) continue;
		const value = coerceSampleValue(arg, raw);
		if (arg.required) {
			// Required args go positional in claude + CLI form.
			claudeTokens.push(String(value));
			cliTokens.push(String(value));
		} else {
			claudeTokens.push(`${arg.name}=${value}`);
			cliTokens.push(`${arg.name}=${value}`);
		}
		mcpArgs[arg.name] = value;
	}

	return {
		nl: `run the ${safeSlug} playbook${claudeTokens.length ? ' ' + claudeTokens.join(' ') : ''}`,
		claude: `/pad ${safeSlug}${claudeTokens.length ? ' ' + claudeTokens.join(' ') : ''}`,
		cli: `pad playbook run ${safeSlug}${cliTokens.length ? ' ' + cliTokens.join(' ') : ''}`,
		mcp: JSON.stringify(
			{ action: 'run', ref: safeSlug, args: mcpArgs },
			null,
			2
		)
	};
}

function coerceSampleValue(arg: PlaybookArgument, raw: string): string | number {
	if (arg.type === 'number') {
		const n = Number(raw);
		if (Number.isFinite(n)) return n;
	}
	return raw;
}
