import { describe, it, expect } from 'vitest';
import { createHeadlessEditor, flushPipeline } from '$lib/collab/materializer/entry';
import { cleanBrokenLinks, markdownToWikiLinks, wikiLinksToMarkdown } from './markdown';
import type { Item } from '$lib/types';

// BUG-4449: a literal `[` earlier in a body must not become the opening bracket
// of a later internal link. Only the link's own text is its label.

const TASK: Item = {
	id: 'id-1',
	title: 'Current title',
	slug: 'fixture',
	collection_slug: 'tasks',
	collection_prefix: 'TASK',
	item_number: 1
} as unknown as Item;

const HREF = '/u/ws/tasks/TASK-1';
const link = (text: string) => `<a href="${HREF}">${text}</a>`;

function withEditor<T>(html: string, use: (md: string, text: string) => T): T {
	const ed = createHeadlessEditor();
	try {
		ed.commands.setContent(html);
		return use((ed.storage as { markdown: { getMarkdown(): string } }).markdown.getMarkdown(), ed.getText());
	} finally {
		ed.destroy();
	}
}

const save = (html: string) => withEditor(html, (md) => flushPipeline(md, [TASK] as never));
const toStored = (md: string) => markdownToWikiLinks(md, [TASK]);

describe('through the real serializer, where a stray bracket arrives escaped', () => {
	it('keeps the text before the link out of its label', () => {
		expect(save(`<p>a b [ c ${link('label')} d</p>`)).toBe('a b \\[ c [[TASK-1|label]] d');
	});

	it('does not cross a paragraph break', () => {
		expect(save(`<p>[ one</p><p>two ${link('label')}</p>`)).toBe('\\[ one\n\ntwo [[TASK-1|label]]');
	});

	it('keeps a real link whose text has brackets', () => {
		expect(save(`<p>${link('a [b] c')}</p>`)).toBe('[[TASK-1|a [b\\] c]]');
	});

	// `[[[TASK-1]]` reads as a wiki link whose body starts with `[`, so a link
	// right after a literal `[` stays a markdown link and survives a reload.
	it('leaves a link directly after a stray bracket as a working link', () => {
		const stored = save(`<p>[${link('label')} d</p>`);
		expect(stored).toBe(`\\[[label](${HREF}) d`);
		const reloaded = wikiLinksToMarkdown(stored, [TASK], 'ws', 'u');
		expect(withEditor(reloaded, (_md, text) => text)).toBe('[label d');
		expect(withEditor(reloaded, (md) => md)).toBe(stored);
	});
});

describe('markdownToWikiLinks label bounds', () => {
	it('stops at an unescaped bracket on one line', () => {
		expect(toStored(`[a b [l](${HREF})`)).toBe('[a b [[TASK-1|l]]');
	});

	it('stops at an unescaped bracket across a line and a paragraph', () => {
		expect(toStored(`[a\nb [l](${HREF})`)).toBe('[a\nb [[TASK-1|l]]');
		expect(toStored(`[a\n\nb [l](${HREF})`)).toBe('[a\n\nb [[TASK-1|l]]');
	});

	it('does not rewrite a table inside the old span', () => {
		const table = '| a | b |\n| --- | --- |\n| 1 | 2 |';
		expect(toStored(`[x\n\n${table}\n\n[l](${HREF})`)).toBe(`[x\n\n${table}\n\n[[TASK-1|l]]`);
	});

	it('applies to cross-workspace links', () => {
		expect(toStored('[a [l](/-/r/other/TASK-9)')).toBe('[a [[other::TASK-9|l]]');
	});

	it('never opens a label at an escaped bracket', () => {
		expect(toStored(`\\[a](${HREF})`)).toBe(`\\[a](${HREF})`);
		expect(toStored('\\[a](/-/r/other/TASK-9)')).toBe('\\[a](/-/r/other/TASK-9)');
	});

	it('opens a label after an escaped backslash', () => {
		expect(toStored(`\\\\[l](${HREF})`)).toBe('\\\\[[TASK-1|l]]');
	});

	it('does not span a blank line', () => {
		expect(toStored(`[a\n\nb](${HREF})`)).toBe(`[a\n\nb](${HREF})`);
	});

	it('keeps a soft line break inside a real label', () => {
		expect(toStored(`[a\nb](${HREF})`)).toBe('[[TASK-1|a\nb]]');
	});

	it('still reads escaped brackets in a real label', () => {
		expect(toStored(`[Use \\[\\[ to link](${HREF})`)).toBe('[[TASK-1|Use [[ to link]]');
	});
});

describe('cleanBrokenLinks label bounds', () => {
	it('converts a placeholder link', () => {
		expect(cleanBrokenLinks('[l](broken)')).toBe('[[l]]');
	});

	it('stops at an unescaped bracket', () => {
		expect(cleanBrokenLinks('[a b [l](broken)')).toBe('[a b [[l]]');
	});

	it('never opens a label at an escaped bracket', () => {
		expect(cleanBrokenLinks('\\[a b [l](broken)')).toBe('\\[a b [[l]]');
	});
});
