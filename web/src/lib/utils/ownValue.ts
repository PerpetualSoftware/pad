/**
 * Look a USER-SUPPLIED key up in an ordinary object literal, answering only
 * what the object itself defines (BUG-3054).
 *
 * A bare `map[key]` walks the prototype chain, so a key like `constructor`,
 * `toString` or `__proto__`, all ordinary strings a user can type as a slug, a
 * status, a file extension, finds an INHERITED member: a function, or
 * `Object.prototype`. `map[key] ?? fallback` does not catch it, because both are
 * truthy. BUG-3041 fixed one such lookup in the status palette; this is the
 * same answer for the rest, so each site does not re-derive it.
 *
 * For a map keyed by user data that the code BUILDS, prefer a null-prototype
 * object or a Map at construction; this is for literals written in source.
 */
export function ownValue<V>(map: Readonly<Record<string, V>>, key: string | null | undefined): V | undefined {
	if (key === null || key === undefined) return undefined;
	return Object.hasOwn(map, key) ? map[key] : undefined;
}
