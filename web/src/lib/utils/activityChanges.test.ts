import { describe, it, expect } from 'vitest';
import { parseFieldChanges, formatChangesForDisplay } from './activityChanges';

// BUG-2628. Two rules, both about what must NOT become a pill:
//
//   - `implementation_notes` / `decision_log` are suppressed, because those
//     entries have had their own timeline cards since BUG-2301 and a pill can
//     only restate the card above it (Dave's ruling, option 3).
//   - a segment whose "field name" is not a field name is not a change at all.
//     The server's change string is joined with "; " and split here on ";", so
//     any value containing a semicolon fragments — and on legacy rows those
//     values are entire notes arrays serialized as Go map literals.
//
// The second rule is the one that does the visible work, and it is easy to
// mistake for defensive garnish. It is not: measured over the 97 legacy rows
// in the live database, suppressing the field names alone removes 77 of 78
// oversized pills and leaves the worst untouched at 2952 characters.

describe('parseFieldChanges', () => {
	it('parses the ordinary case unchanged', () => {
		expect(parseFieldChanges('status: open → fixing; priority: low → high')).toEqual([
			{ field: 'status', from: 'open', to: 'fixing' },
			{ field: 'priority', from: 'low', to: 'high' },
		]);
	});

	it('keeps every field the writer emits that is not suppressed', () => {
		// title / role / assigned come from appendChange rather than the schema,
		// so they are the cases a schema-key-shaped guard could wrongly refuse.
		// One-sided values ("field: → value") are KEPT with an empty `from` —
		// checked against the function, after the source comment claiming they
		// were dropped turned out to be wrong and sent this expectation astray.
		const parsed = parseFieldChanges(
			'title: Old → New; role: → Implementer; assigned: Dave → Wren; due_date: → 2026-09-01',
		);
		expect(parsed).toEqual([
			{ field: 'title', from: 'Old', to: 'New' },
			{ field: 'role', from: '', to: 'Implementer' },
			{ field: 'assigned', from: 'Dave', to: 'Wren' },
			{ field: 'due_date', from: '', to: '2026-09-01' },
		]);
	});

	it('suppresses the two structured-entry fields', () => {
		const parsed = parseFieldChanges(
			'status: open → done; implementation_notes: (1 note) → (2 notes); decision_log: (1 entry) → (2 entries)',
		);
		expect(parsed).toEqual([{ field: 'status', from: 'open', to: 'done' }]);
	});

	it('suppresses them in the summarized form current writes produce', () => {
		expect(parseFieldChanges('implementation_notes: → (1 note)')).toEqual([]);
	});

	// The regression case, shaped from a real legacy row: the whole notes array
	// as a Go map literal, containing the ";" the parser splits on, a ":" and
	// an arrow inside the note's own prose. Before the fix this rendered as a
	// wall of text; suppressing only the field name leaves the FRAGMENT, whose
	// apparent field is the prose before its colon.
	it('removes the wall from a legacy Go-map-literal notes blob', () => {
		const legacy =
			'implementation_notes: → [map[created_at:2026-04-02T18:17:50Z created_by:user ' +
			'details:Root groups: auth, server; also see `foo: a → b` in the notes ' +
			'id:note-1775153870894988317 summary:Proposed first-release CLI grouping]]; ' +
			'status: open → done';
		const parsed = parseFieldChanges(legacy);
		// The real change on the same card survives...
		expect(parsed).toEqual([{ field: 'status', from: 'open', to: 'done' }]);
		// ...and nothing carrying the blob does.
		for (const c of parsed) {
			expect(c.from.length + c.to.length).toBeLessThan(100);
		}
	});

	// The guard's OTHER error direction, pinned rather than left implicit: a
	// fragment whose prose before the colon happens to be a single word still
	// produces a pill. The guard removes the wall; it does not promise zero
	// fragments, and no shape test could — the server joins with "; " and this
	// splits on ";", so a value containing a semicolon is indistinguishable
	// from two changes. IDEA-2790 is the durable fix (structured metadata).
	it('does NOT catch a fragment whose apparent key is a single word', () => {
		const parsed = parseFieldChanges('implementation_notes: → [map[details:Root; foo: a → b]]');
		expect(parsed).toEqual([{ field: 'foo', from: 'a', to: 'b]]' }]);
		// What matters is that the WALL is gone: the surviving pill is small.
		expect(parsed[0].from.length + parsed[0].to.length).toBeLessThan(20);
	});

	it('refuses a segment whose field name is prose rather than a key', () => {
		// This is the fragment the field-name suppression cannot catch, because
		// the apparent field is not `implementation_notes` — it is whatever text
		// preceded the colon in the note body.
		const fragment = 'also see `foo` and a longer line of markdown prose: a → b';
		expect(parseFieldChanges(fragment)).toEqual([]);
	});

	it('refuses spaced and empty field names but ACCEPTS unusual legal keys', () => {
		expect(parseFieldChanges('two words: a → b')).toEqual([]);
		expect(parseFieldChanges(': a → b')).toEqual([]);
		// Nothing constrains a collection's field keys to lowercase — the
		// server compares them with a plain `==`, no case folding — so these
		// are legal keys and their pills must survive. An earlier version of
		// the guard was a lowercase-identifier pattern and would have dropped
		// them silently; every key in the live database happens to satisfy
		// both forms, so the measurement could not tell them apart.
		expect(parseFieldChanges('Status: open → done')).toEqual([
			{ field: 'Status', from: 'open', to: 'done' },
		]);
		expect(parseFieldChanges('resolution-v2: a → b')).toEqual([
			{ field: 'resolution-v2', from: 'a', to: 'b' },
		]);
	});

	it('handles empty and absent input', () => {
		expect(parseFieldChanges('')).toEqual([]);
		expect(parseFieldChanges(null)).toEqual([]);
		expect(parseFieldChanges(undefined)).toEqual([]);
	});

	// The surfaces that render the change string as TEXT rather than as pills.
	// The audit page's fallback is the sharp one: it renders raw when the
	// parsed list is empty, so suppressing every pill on a legacy row put the
	// wall of text back on the very surface the ruling was about.
	describe('formatChangesForDisplay', () => {
		it('drops suppressed fields and keeps the rest verbatim', () => {
			expect(
				formatChangesForDisplay('status: open → done; implementation_notes: (1 note) → (2 notes)'),
			).toBe('status: open → done');
		});

		it('returns empty when the only change was a suppressed field', () => {
			// This is the case that triggers the audit page's raw fallback.
			// It must produce nothing to render, not the raw string.
			expect(formatChangesForDisplay('implementation_notes: → (1 note)')).toBe('');
		});

		it('drops a legacy Go-map-literal blob entirely', () => {
			const legacy =
				'implementation_notes: → [map[created_at:2026-04-02T18:17:50Z created_by:user ' +
				'details:Root groups: auth, server; also see `foo: a → b` in the notes ' +
				'id:note-1775153870894988317 summary:Proposed first-release CLI grouping]]; ' +
				'status: open → done';
			expect(formatChangesForDisplay(legacy)).toBe('status: open → done');
		});

		it('normalizes separators and trims, rather than preserving bytes', () => {
			// Named for what it does. Survivors keep their CONTENT, but the
			// rejoin is canonical "; " and each segment is trimmed.
			expect(formatChangesForDisplay('status:  open → done ;priority: low → high')).toBe(
				'status:  open → done; priority: low → high',
			);
		});

		it('loses the tail of a legitimate value containing a semicolon', () => {
			// The format ambiguity from the other side, pinned so it is a known
			// limit rather than a surprise: "a; b" cannot be told from two
			// changes, and the orphaned tail fails the field-key rule.
			expect(formatChangesForDisplay('status: open → a; b')).toBe('status: open → a');
		});

		it('keeps a segment parseFieldChanges will not turn into a pill', () => {
			// More than one arrow: no pill, but it is a real change and the
			// fallback exists to show exactly this.
			const s = 'status: a → b → c; priority: low → high';
			expect(parseFieldChanges(s)).toEqual([{ field: 'priority', from: 'low', to: 'high' }]);
			expect(formatChangesForDisplay(s)).toBe('status: a → b → c; priority: low → high');
		});

		it('handles empty and absent input', () => {
			expect(formatChangesForDisplay('')).toBe('');
			expect(formatChangesForDisplay(null)).toBe('');
			expect(formatChangesForDisplay(undefined)).toBe('');
		});
	});
});
