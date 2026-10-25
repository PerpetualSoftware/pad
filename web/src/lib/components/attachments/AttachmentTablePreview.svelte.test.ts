import { describe, it, expect, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import AttachmentTablePreview from './AttachmentTablePreview.svelte';

let component: ReturnType<typeof mount> | undefined;
afterEach(async () => {
	if (component) await unmount(component);
	component = undefined;
	document.body.replaceChildren();
});

function preview(text: string, mime = 'text/csv') {
	component = mount(AttachmentTablePreview, { target: document.body, props: { text, mime } });
	flushSync();
}

describe('AttachmentTablePreview', () => {
	it('renders hostile HTML and spreadsheet formulas as literal text, with no image or link', () => {
		preview('name,value\n<img src=x onerror=alert(1)>,"=HYPERLINK(""x"")"');
		const cells = document.querySelectorAll('td');
		expect(cells[0]?.textContent).toBe('<img src=x onerror=alert(1)>');
		expect(cells[1]?.textContent).toBe('=HYPERLINK("x")');
		expect(document.querySelector('img')).toBeNull();
		expect(document.querySelector('a')).toBeNull();
		expect(document.querySelectorAll('th[scope="col"]')).toHaveLength(2);
	});

	it('reports exact row and column totals even when the widest record is past the row cap', () => {
		preview(Array.from({ length: 1002 }, (_, i) => Array(i === 1001 ? 53 : 2).fill('x').join(',')).join('\n'));
		expect(document.querySelectorAll('tr')).toHaveLength(1000);
		expect(document.querySelectorAll('th')).toHaveLength(50);
		expect(document.querySelector('[role="status"]')?.textContent).toContain('Showing 1000 of 1002 rows');
		expect(document.querySelector('[role="status"]')?.textContent).toContain('50 of 53 columns');
	});

	it('chooses tab from the TSV MIME and shows a partial parse error below the table', () => {
		preview('one\ttwo\nfirst\tsecond\n"unfinished', 'text/tab-separated-values');
		expect([...document.querySelectorAll('td')].map((cell) => cell.textContent)).toEqual(['first', 'second']);
		expect(document.querySelector('.attachment-table-error')?.textContent).toContain('Could not parse past row 2');
	});

	it('shows an empty table status without inventing a header', () => {
		preview('');
		expect(document.querySelector('th')).toBeNull();
		expect(document.querySelector('[role="status"]')?.textContent).toContain('Showing 0 of 0 rows');
	});
});
