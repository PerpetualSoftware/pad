import { describe, it, expect } from 'vitest';
import { parseDelimited, ROW_CAP, COL_CAP, CELL_CAP } from './csv';

describe('attachment CSV parser', () => {
	it('keeps quoted delimiters, escaped quotes and embedded CRLF in one field', () => {
		expect(parseDelimited('name,note\r\n"a,b","say ""hi""\r\nagain"\r\n', ',').rows)
			.toEqual([['name', 'note'], ['a,b', 'say "hi"\r\nagain']]);
	});

	it('strips a leading BOM rather than displaying it in the first header', () => {
		expect(parseDelimited('\uFEFFname,value\nx,1', ',').rows)
			.toEqual([['name', 'value'], ['x', '1']]);
	});

	it('uses the supplied TSV delimiter, with LF and ragged rows preserved', () => {
		expect(parseDelimited('a\tb\nc\n"d\te"\tf\tg', '\t').rows)
			.toEqual([['a', 'b'], ['c'], ['d\te', 'f', 'g']]);
	});

	it('does not invent a record for an empty file or a final newline', () => {
		expect(parseDelimited('', ',')).toEqual({ rows: [], totalRows: 0, totalColumns: 0, error: null });
		expect(parseDelimited('a,b\n', ',').totalRows).toBe(1);
		expect(parseDelimited('a,', ',').rows).toEqual([['a', '']]);
	});

	it('T6: skips leading, intervening and trailing blank lines without counting them, keeping delimiter-only and quoted records', () => {
		const source = '\r\n\nname\r\n\r\nvalue\n\n,,\r\n"line\n\nbreak"\r\n""\r\n\r\n';
		expect(parseDelimited(source, ',')).toEqual({
			rows: [['name'], ['value'], ['', '', ''], ['line\n\nbreak'], ['']],
			totalRows: 5, totalColumns: 3, error: null
		});
	});

	it('returns complete records and an error without throwing on an unterminated quote at EOF', () => {
		let result: ReturnType<typeof parseDelimited> | undefined;
		expect(() => { result = parseDelimited('a,b\nx,y\n"unfinished', ','); }).not.toThrow();
		expect(result).toEqual({ rows: [['a', 'b'], ['x', 'y']], totalRows: 2, totalColumns: 2, error: { row: 2 } });
	});

	it('S1: keeps mid-field quotes and characters after a closing quote literally, with RFC escaped quotes intact', () => {
		expect(parseDelimited('name,note\n5" monitor,a"b\n"abc"def,"say ""hi"""', ',')).toEqual({
			rows: [['name', 'note'], ['5" monitor', 'a"b'], ['abcdef', 'say "hi"']],
			totalRows: 3, totalColumns: 2, error: null
		});
	});

	it('caps retained rows at exactly 1000 while counting records and columns past both caps', () => {
		// The header is the first record, so 1000 retained records means at most
		// 50,000 displayed cells INCLUDING the header. The widest record is last:
		// stopping the scan at ROW_CAP lies about both totals.
		const source = Array.from({ length: ROW_CAP + 2 }, (_, i) =>
			Array.from({ length: i === ROW_CAP + 1 ? COL_CAP + 3 : 2 }, () => String(i)).join(',')
		).join('\n');
		const result = parseDelimited(source, ',');
		expect(ROW_CAP).toBe(1000);
		expect(COL_CAP).toBe(50);
		expect(result.rows).toHaveLength(1000);
		expect(result.rows.at(-1)).toEqual(['999', '999']);
		expect(result.totalRows).toBe(1002);
		expect(result.totalColumns).toBe(53);
		expect(result.error).toBeNull();
		expect(parseDelimited(Array(53).fill('x').join(','), ',').rows[0]).toHaveLength(50);
	});

	it('U4: never splits a surrogate pair at the cell cap and retains the ellipsis', () => {
		const source = 'a'.repeat(499) + '😀' + 'tail';
		for (const cell of [source, `"${source}"`]) {
			const result = parseDelimited(`${cell},next\nlast,row`, ',');
			const retained = result.rows[0][0];
			expect(retained.endsWith('…')).toBe(true);
			expect(retained.slice(0, -1)).not.toMatch(/[\uD800-\uDBFF]$/);
			expect(retained).toBe('a'.repeat(499) + '😀…');
			expect(result.rows).toEqual([[retained, 'next'], ['last', 'row']]);
			expect(result.totalRows).toBe(2);
			expect(result.totalColumns).toBe(2);
			expect(result.error).toBeNull();
		}
	});

	it('truncates displayed cells at 500 characters with an ellipsis', () => {
		expect(CELL_CAP).toBe(500);
		expect(parseDelimited('x'.repeat(501), ',').rows[0][0]).toBe('x'.repeat(500) + '…');
		expect(parseDelimited('x'.repeat(500), ',').rows[0][0]).toBe('x'.repeat(500));
	});
});
