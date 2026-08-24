import { describe, it, expect } from 'vitest';
import { agentNameFromMetadata, agentNameOf } from './agentActor';

/**
 * The helper's whole contract is "verbatim, or nothing" — so the cases that
 * matter are the ones a filter or a normalizer would change. TASK-2759
 * replaced exactly such a filter (GENERIC_AGENT_IDS), and these assert the
 * shapes it used to swallow.
 */
describe('agentNameFromMetadata', () => {
	it('returns the stamped name verbatim', () => {
		expect(agentNameFromMetadata('{"agent":"wren"}')).toBe('wren');
	});

	it.each([
		// The three ids the retired shim filtered. They are here as named
		// cases, not as a list to re-suppress: whichever way this function
		// changes, someone should have to look at these on purpose.
		'claude-code',
		'cli',
		'agent',
		// Shapes a normalizer would mangle: casing, inner spacing, unicode.
		'Claude-Code',
		'my agent',
		'wren-2',
		'ロボット'
	])('does not filter or transform %s', (name) => {
		expect(agentNameFromMetadata(JSON.stringify({ agent: name }))).toBe(name);
	});

	it('preserves surrounding whitespace rather than trimming it', () => {
		// Trimming is a normalization, and the only writer (the CLI's
		// X-Pad-Agent header) is already OWS-stripped by Go's header parser
		// before the server stamps it — so a trim here would only ever act on
		// a value some other client deliberately sent.
		expect(agentNameFromMetadata('{"agent":" wren "}')).toBe(' wren ');
	});

	it.each([
		['no agent key', '{"changes":"status: open -> done"}'],
		['an empty name', '{"agent":""}'],
		['a null name', '{"agent":null}'],
		['a numeric name', '{"agent":123}'],
		['an object name', '{"agent":{"name":"wren"}}'],
		['unparseable metadata', 'not json'],
		['an empty string', ''],
		['undefined', undefined],
		['null', null]
	])('returns undefined for %s', (_case, metadata) => {
		expect(agentNameFromMetadata(metadata as string | undefined | null)).toBeUndefined();
	});

	it('reads the same name as the parsed-object form', () => {
		const raw = '{"agent":"rook","changes":"status"}';
		expect(agentNameFromMetadata(raw)).toBe(agentNameOf(JSON.parse(raw)));
	});
});

// Four of the five surfaces call this one, not the string form, because they
// already hold the parsed metadata. Equivalence with agentNameFromMetadata was
// the only thing pinning it, which covers the shared path and not this
// function's own edges — most of them only existed incidentally inside
// component tests (codex round 14).
describe('agentNameOf', () => {
	it('returns the name from a parsed object', () => {
		expect(agentNameOf({ agent: 'wren', changes: 'status' })).toBe('wren');
	});

	it.each([
		['no agent key', { changes: 'status' }],
		['an empty name', { agent: '' }],
		['a null name', { agent: null }],
		['a numeric name', { agent: 123 }],
		['an object name', { agent: { name: 'wren' } }],
		['an empty object', {}],
		['undefined', undefined],
		['null', null]
	])('returns undefined for %s', (_case, meta) => {
		expect(agentNameOf(meta as Record<string, unknown> | undefined | null)).toBeUndefined();
	});

	it('does not filter a generic-looking id', () => {
		expect(agentNameOf({ agent: 'claude-code' })).toBe('claude-code');
	});

	it('takes the last value when the stamp was spliced onto existing metadata', () => {
		// agentMeta merges by string splice (handlers_documents.go:287), so a
		// row whose metadata already carried an `agent` key arrives with a
		// duplicate. JSON.parse resolves that last-wins; this pins the
		// behaviour so a future switch to a hand-rolled parser cannot change
		// which name a reader sees without a test failing.
		expect(agentNameFromMetadata('{"agent":"spliced","agent":"original"}')).toBe('original');
	});
});

// The `agentActorLabel(metadata, fallback)` wrapper these last cases covered
// was removed in codex round 3: one caller, and it hid the `?? fallback` that
// every other site wrote inline. Each surface's own fallback is asserted where
// that surface is tested, which is where the casing difference actually lives.
