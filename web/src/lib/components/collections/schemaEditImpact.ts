import type { CollectionFieldUsage, FieldMigration } from '$lib/types';

/**
 * TASK-2188 / TASK-2187: what a schema edit does to items that already hold
 * values, worked out before the save so the editor can say so.
 *
 * Three kinds, each carrying how many live items it touches (`items`
 * undefined when the count could not be loaded, which callers treat as
 * "could be any number", never as none):
 * - `field`: an existing field was removed. Its values stay stored, hidden,
 *   and come back if a field with that key is declared again.
 * - `option`: a select/multi_select option was removed from a field that is
 *   still one. Its items keep the value, which no longer matches an option.
 * - `rename`: an option was renamed, so its items move to the new value.
 *
 * The rules match the server's `warnings.orphaned` (store.OrphanedBySchemaEdit):
 * a type change away from select is neither, and a removal nobody's item
 * holds is not reported.
 */
export type SchemaEditImpact =
	| { kind: 'field'; field: string; label: string; items: number | undefined }
	| { kind: 'option'; field: string; label: string; option: string; items: number | undefined }
	| { kind: 'rename'; field: string; label: string; from: string; to: string; items: number | undefined };

/** A field as the editor was seeded with it, before any edit. */
export interface SeededField {
	key: string;
	label: string;
	type: string;
	options: string[];
}

/** The parts of an edited field this needs. */
export interface EditedField {
	key: string;
	type: string;
	options: string[];
}

const isOptionType = (t: string) => t === 'select' || t === 'multi_select';

export function schemaEditImpacts(
	seeded: readonly SeededField[],
	edited: readonly EditedField[],
	migrations: readonly FieldMigration[],
	usage: CollectionFieldUsage | null
): SchemaEditImpact[] {
	const editedByKey = new Map(edited.map((f) => [f.key, f]));
	const out: SchemaEditImpact[] = [];
	const countField = (key: string) => (usage ? (usage.fields[key]?.items ?? 0) : undefined);
	const countValue = (key: string, value: string) =>
		usage ? (usage.fields[key]?.values?.[value] ?? 0) : undefined;
	const held = (n: number | undefined) => n === undefined || n > 0;

	for (const s of seeded) {
		const e = editedByKey.get(s.key);
		if (!e) {
			const items = countField(s.key);
			if (held(items)) out.push({ kind: 'field', field: s.key, label: s.label, items });
			continue;
		}
		if (!isOptionType(s.type) || !isOptionType(e.type)) continue;
		const renames = migrations.find((m) => m.field === s.key)?.rename_options ?? {};
		const kept = new Set(e.options);
		for (const option of s.options) {
			if (Object.prototype.hasOwnProperty.call(renames, option)) {
				const items = countValue(s.key, option);
				if (held(items)) {
					out.push({ kind: 'rename', field: s.key, label: s.label, from: option, to: renames[option], items });
				}
				continue;
			}
			if (kept.has(option)) continue;
			const items = countValue(s.key, option);
			if (held(items)) out.push({ kind: 'option', field: s.key, label: s.label, option, items });
		}
	}
	return out;
}

/** A save that would leave this many items behind asks for the name typed. */
export const TYPED_SCHEMA_EDIT_THRESHOLD = 25;

/** Whether the confirm must ask for the collection name typed: a large total, or any count unknown. */
export function schemaEditNeedsTyping(impacts: readonly SchemaEditImpact[]): boolean {
	let total = 0;
	for (const i of impacts) {
		if (i.items === undefined) return true;
		total += i.items;
	}
	return total >= TYPED_SCHEMA_EDIT_THRESHOLD;
}

const itemsPhrase = (n: number | undefined) =>
	n === undefined ? 'An unknown number of items' : n === 1 ? '1 item' : `${n} items`;

/** One sentence per impact, true to what the server does with it. */
export function describeSchemaEditImpact(i: SchemaEditImpact): string {
	const who = itemsPhrase(i.items);
	const have = i.items === 1 ? 'has' : 'have';
	switch (i.kind) {
		case 'field':
			return `${who} ${have} a value in “${i.label}”. Removing the field hides it; the values stay stored and come back if a field with the key “${i.field}” is added again.`;
		case 'option':
			return `${who} ${have} “${i.option}” in “${i.label}”. They keep it, but it stops being a valid option, so “${i.label}” has to be set to a listed option the next time it is changed. Rename the option instead to move them.`;
		case 'rename':
			return `${who} ${i.items === 1 ? 'moves' : 'move'} from “${i.from}” to “${i.to}” in “${i.label}”.`;
	}
}
