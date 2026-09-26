// What the playbook editor writes on save (BUG-3075).
//
// The editor used to send status, trigger, scope, arguments and
// invocation_slug on EVERY save. Its load replaces a stored value the controls
// cannot hold with a default (a non-string status loads as `draft`), so an
// untouched field was OVERWRITTEN: the silent coercing write the BUG-3052
// ruling forbids (a stored value that does not fit is shown and replaced
// explicitly, never coerced). A save now sends only the keys the user changed.
//
// Extracted from the route so it can be tested: the page cannot be mounted
// without the whole workspace shell (see bug3049WebFullBlobWriters.test.ts).
import { rawText } from '$lib/fields/fieldShape';

/** The editor's form values, as held at load and at save. `args` is argumentsToJSON's string. */
export type PlaybookFormSnapshot = {
	status: string;
	trigger: string;
	scope: string;
	invocationSlug: string;
	args: string;
};

/**
 * The `fields_patch` a save sends: the keys whose form value differs from what
 * loaded, and only those. With no load snapshot (nothing to compare against),
 * every key is sent, which is the old behaviour.
 */
export function playbookFieldsPatch(
	now: PlaybookFormSnapshot,
	loaded: PlaybookFormSnapshot | null,
): Record<string, unknown> {
	const patch: Record<string, unknown> = {};
	if (!loaded || now.status !== loaded.status) patch.status = now.status;
	if (!loaded || now.trigger !== loaded.trigger) patch.trigger = now.trigger;
	if (!loaded || now.scope !== loaded.scope) patch.scope = now.scope;
	// `arguments` goes in as a JSON VALUE (array of objects), not a stringified
	// array: the server stores it as a `json` field
	// (internal/collections/templates_startup_ship.go has the canonical shape).
	if (!loaded || now.args !== loaded.args) patch.arguments = JSON.parse(now.args);
	// An empty slug clears the field; a non-empty one sets it. The clear is a
	// NULL, not `""` (which would still hit the unique index): the store's
	// mergeFieldsPatch removes a NULL key from the stored blob.
	const trimmedSlug = now.invocationSlug.trim();
	if (!loaded || trimmedSlug !== loaded.invocationSlug.trim()) {
		patch.invocation_slug = trimmedSlug ? trimmedSlug : null;
	}
	return patch;
}

/**
 * A stored status / trigger / scope the form cannot show as itself: not a
 * string (any of the three), or not one of the field's options (status and
 * scope; a trigger may be custom by design). The form shows a default in its
 * place, so the page says what is actually stored and that it is kept.
 */
export function storedFormMismatches(
	stored: { status: unknown; trigger: unknown; scope: unknown },
	statuses: readonly string[],
	scopes: readonly string[],
): { label: string; raw: string }[] {
	const out: { label: string; raw: string }[] = [];
	const check = (label: string, raw: unknown, options: readonly string[] | null) => {
		if (raw == null || raw === '') return;
		if (typeof raw !== 'string' || (options && !options.includes(raw))) out.push({ label, raw: rawText(raw) });
	};
	check('Status', stored.status, statuses);
	check('Trigger', stored.trigger, null);
	check('Scope', stored.scope, scopes);
	return out;
}
