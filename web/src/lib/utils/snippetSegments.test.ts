import { describe, expect, it } from 'vitest';
import { snippetSegments, windowSnippet } from './snippetSegments';

const text = (segs: { text: string }[]) => segs.map((s) => s.text).join('');

describe('snippetSegments', () => {
	it('plain text is one non-hit segment', () => {
		expect(snippetSegments('just words')).toEqual([{ text: 'just words', hit: false }]);
	});

	it('an empty snippet has no segments', () => {
		expect(snippetSegments('')).toEqual([]);
	});

	it('one marked term is a hit between plain text', () => {
		expect(snippetSegments('foo <mark>leap</mark> bar')).toEqual([
			{ text: 'foo ', hit: false },
			{ text: 'leap', hit: true },
			{ text: ' bar', hit: false },
		]);
	});

	it('several marked terms are each a hit', () => {
		expect(snippetSegments('<mark>a</mark>, b, <mark>c</mark>')).toEqual([
			{ text: 'a', hit: true },
			{ text: ', b, ', hit: false },
			{ text: 'c', hit: true },
		]);
	});

	it('other tags are stripped as before', () => {
		expect(snippetSegments('<b>x</b> y')).toEqual([{ text: 'x y', hit: false }]);
	});

	it('a script tag leaves only inert text, no tag survives', () => {
		const segs = snippetSegments('a<script>alert(1)</script> <img src=x onerror=alert(1)><mark>b</mark>');
		expect(text(segs)).toBe('aalert(1) b');
		for (const s of segs) expect(s.text).not.toMatch(/[<>]/);
	});

	it('an unclosed mark highlights to the end', () => {
		expect(snippetSegments('foo <mark>leap bar')).toEqual([
			{ text: 'foo ', hit: false },
			{ text: 'leap bar', hit: true },
		]);
	});

	it('a stray closing mark is dropped', () => {
		expect(snippetSegments('foo </mark>bar')).toEqual([{ text: 'foo bar', hit: false }]);
	});
});

describe('windowSnippet', () => {
	it('leaves a hit near the start alone', () => {
		const segs = snippetSegments('the <mark>leap</mark> year rule');
		expect(windowSnippet(segs, 24)).toEqual(segs);
	});

	it('leaves a snippet with no hit alone', () => {
		const segs = snippetSegments('x'.repeat(100));
		expect(windowSnippet(segs, 24)).toEqual(segs);
	});

	it('trims a long lead so a hit at char 80 sits near the start', () => {
		const lead = 'word '.repeat(16); // 80 chars
		const segs = snippetSegments(`${lead}<mark>digest</mark> tail`);
		const out = windowSnippet(segs, 24);
		const joined = text(out);
		expect(joined.startsWith('…')).toBe(true);
		const hitAt = joined.indexOf('digest');
		expect(hitAt).toBeGreaterThan(0);
		expect(hitAt).toBeLessThanOrEqual(30);
		expect(out.find((s) => s.hit)?.text).toBe('digest');
		expect(joined.endsWith('digest tail')).toBe(true);
	});

	it('trims at a word boundary, never mid-word', () => {
		const segs = snippetSegments(
			'Harbour master asked whether the digest cadence could <mark>shift</mark> later',
		);
		const out = windowSnippet(segs, 24);
		expect(out[0]).toEqual({ text: '…digest cadence could ', hit: false });
		expect(out[1]).toEqual({ text: 'shift', hit: true });
	});
});
