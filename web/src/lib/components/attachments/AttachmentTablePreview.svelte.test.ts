import { describe, it, expect, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import AttachmentTablePreview from './AttachmentTablePreview.svelte';

let component: ReturnType<typeof mount> | undefined;
afterEach(async () => {
	if (component) await unmount(component);
	component = undefined;
	document.body.replaceChildren();
});

function preview(text: string, mime = 'text/csv', compact = false) {
	component = mount(AttachmentTablePreview, { target: document.body, props: { text, mime, compact } });
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

	function more() {
		return [...document.querySelectorAll('button')].find((b) => b.textContent?.trim() === 'Show more rows');
	}
	function clickMore() {
		expect(more()).toBeDefined();
		more()!.click();
		flushSync();
	}

	// This case deliberately reaches the 50,000-cell ceiling across every step.
	// Allow jsdom's mount/teardown cost under the FULL suite's contention; this
	// asserts exact counts, not a wall-clock performance budget.
	it('reports exact row and column totals after EVERY step even when the widest record is past the ceiling', () => {
		preview(Array.from({ length: 1002 }, (_, i) => Array(i === 1001 ? 53 : 2).fill('x').join(',')).join('\n'));
		// Ragged records are padded to the rendered width: the budget must count
		// those cells too. The header counts as a mounted row, not free work.
		for (let rows = 100; rows <= 1000; rows += 100) {
			expect(document.querySelectorAll('tr')).toHaveLength(rows);
			expect(document.querySelectorAll('th')).toHaveLength(50);
			expect(document.querySelector('[role="status"]')?.textContent).toContain(`Showing ${rows} of 1002 rows`);
			expect(document.querySelector('[role="status"]')?.textContent).toContain('50 of 53 columns');
			if (rows < 1000) clickMore();
		}
		expect(more()).toBeUndefined();
	}, 15_000);

	it('bounds the desktop first mount to 5000 cells: 100 rows at 50 columns', () => {
		preview(Array(1001).fill(Array(50).fill('x').join(',')).join('\n'));
		expect(document.querySelectorAll('tr')).toHaveLength(100);
		expect(document.querySelectorAll('th,td')).toHaveLength(5000);
	});

	it('allows all 1000 narrow rows at 5 columns in the first desktop step', () => {
		preview(Array(1000).fill('a,b,c,d,e').join('\n'));
		expect(document.querySelectorAll('tr')).toHaveLength(1000);
		expect(more()).toBeUndefined();
	});

	it('APPENDS each step without remounting any existing tr node and hides the button at N == M', () => {
		preview(Array(205).fill(Array(50).fill('x').join(',')).join('\n'));
		const first = [...document.querySelectorAll('tr')];
		expect(first).toHaveLength(100);
		clickMore();
		const second = [...document.querySelectorAll('tr')];
		expect(second).toHaveLength(200);
		first.forEach((node, i) => expect(second[i]).toBe(node));
		expect(document.querySelector('[role="status"]')?.textContent).toContain('Showing 200 of 205 rows');
		clickMore();
		const last = [...document.querySelectorAll('tr')];
		expect(last).toHaveLength(205);
		second.forEach((node, i) => expect(last[i]).toBe(node));
		expect(document.querySelector('[role="status"]')?.textContent).toContain('Showing 205 of 205 rows');
		expect(more()).toBeUndefined();
	});

	it.each(['document', 'layout'])('U5: resets expanded rows to the first step on a new %s', (change) => {
		const props = $state({
			text: Array(220).fill(Array(50).fill('old').join(',')).join('\n'),
			mime: 'text/csv',
			compact: false
		});
		component = mount(AttachmentTablePreview, { target: document.body, props });
		flushSync();
		expect(document.querySelectorAll('tr')).toHaveLength(100);
		clickMore();
		expect(document.querySelectorAll('tr')).toHaveLength(200);
		if (change === 'document') {
			props.text = Array(210).fill(Array(50).fill('new').join(',')).join('\n');
		} else {
			props.compact = true;
		}
		flushSync();
		const firstStep = change === 'document' ? 100 : 50;
		const total = change === 'document' ? 210 : 220;
		expect(document.querySelectorAll('tr')).toHaveLength(firstStep);
		expect(document.querySelector('[role="status"]')?.textContent).toContain(`Showing ${firstStep} of ${total} rows`);
		if (change === 'document') expect(document.querySelector('td')?.textContent).toBe('new');
		expect(more()).toBeDefined();
	});

	it('uses the phone layout prop to bound the first mount and each step to 2500 cells', () => {
		preview(Array(101).fill(Array(50).fill('x').join(',')).join('\n'), 'text/csv', true);
		expect(document.querySelectorAll('tr')).toHaveLength(50);
		expect(document.querySelectorAll('th,td')).toHaveLength(2500);
		expect(document.querySelector('[role="status"]')?.textContent).toContain('Showing 50 of 101 rows');
		clickMore();
		expect(document.querySelectorAll('tr')).toHaveLength(100);
		expect(document.querySelector('[role="status"]')?.textContent).toContain('Showing 100 of 101 rows');
		clickMore();
		expect(document.querySelectorAll('tr')).toHaveLength(101);
		expect(document.querySelector('[role="status"]')?.textContent).toContain('Showing 101 of 101 rows');
		expect(more()).toBeUndefined();
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
