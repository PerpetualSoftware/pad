/** Display bounds, not parsing bounds: totals still scan the entire byte-bounded document. */
export const ROW_CAP = 1000;
export const COL_CAP = 50;
export const CELL_CAP = 500;

export interface DelimitedPreview {
	/** Complete records only; the first record is the header and counts toward ROW_CAP. */
	rows: string[][];
	totalRows: number;
	/** Widest complete record, including records beyond the display cap. */
	totalColumns: number;
	/** Last complete record (zero if none), for "could not parse past row N". */
	error: { row: number } | null;
}

/**
 * RFC 4180 fields, also accepting LF records and a MIME-selected tab delimiter.
 * An unterminated quoted field returns complete records so far, never an exception.
 * Ragged records are retained: a missing cell is different from a parse failure.
 *
 * WHY KEEP SCANNING AFTER THE CAPS. A preview must tell the truth about what it
 * hides. Stopping at 1000 records cannot produce "N of M rows", and a wider
 * record at the end can change the column total too. Only retained strings and
 * arrays are capped; the state machine still consumes every character, including
 * quoted newlines, so the totals count records rather than physical lines.
 * The caller must enforce the byte bound before invoking this synchronous scan.
 */
export function parseDelimited(source: string, delimiter: ',' | '\t'): DelimitedPreview {
	const result: DelimitedPreview = { rows: [], totalRows: 0, totalColumns: 0, error: null };
	let row: string[] = [];
	let columns = 0;
	let field = '';
	let fieldLength = 0;
	let quoted = false;
	let closed = false;
	let recordStarted = false;

	function append(char: string) {
		// Count code points, preserving surrogate pairs (not grapheme clusters).
		// Keep scanning all characters, but allocate only what can be displayed.
		if (result.totalRows < ROW_CAP && columns < COL_CAP && fieldLength < CELL_CAP) field += char;
		fieldLength++;
	}
	function finishField() {
		if (result.totalRows < ROW_CAP && columns < COL_CAP) {
			row.push(field + (fieldLength > CELL_CAP ? '…' : ''));
		}
		columns++;
		field = '';
		fieldLength = 0;
		closed = false;
	}
	function finishRecord() {
		finishField();
		if (result.totalRows < ROW_CAP) result.rows.push(row);
		result.totalRows++;
		result.totalColumns = Math.max(result.totalColumns, columns);
		row = [];
		columns = 0;
		recordStarted = false;
	}
	function malformed() {
		result.error = { row: result.totalRows };
		return result;
	}

	for (let i = source.charCodeAt(0) === 0xfeff ? 1 : 0; i < source.length; i++) {
		const char = String.fromCodePoint(source.codePointAt(i)!);
		i += char.length - 1;
		if (quoted) {
			if (char === '"') {
				if (source[i + 1] === '"') {
					append('"');
					i++;
				} else {
					quoted = false;
					closed = true;
				}
			} else append(char);
			continue;
		}
		if (char === delimiter) {
			finishField();
			recordStarted = true;
		} else if (char === '\r' || char === '\n') {
			// A bare line ending outside quotes is not a record. Delimiters
			// and opening quotes set recordStarted, even for empty cells.
			if (recordStarted) finishRecord();
			if (char === '\r' && source[i + 1] === '\n') i++;
		} else if (char === '"') {
			// Only a quote at the start of a field opens RFC quoting. Elsewhere
			// it is literal. After a closing quote, append any suffix literally
			// (including quotes): leniently, "abc"def becomes abcdef.
			if (fieldLength !== 0 || closed) append(char);
			else quoted = true;
			recordStarted = true;
		} else {
			append(char);
			recordStarted = true;
		}
	}
	if (quoted) return malformed();
	if (recordStarted) finishRecord();
	return result;
}
