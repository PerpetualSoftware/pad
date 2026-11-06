import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';

const source = (file: string) => readFileSync(new URL(file, import.meta.url), 'utf8')
	.replace(/<!--[\s\S]*?-->/g, '');
const cardEditors = [...source('./TimelineCommentCard.svelte').matchAll(/<CommentEditor\b[\s\S]*?\/>/g)].map(match => match[0]);

describe('comment editor caller names (BUG-4336)', () => {
	it.each([
		[0, 'Edit comment', 'content={comment.body}'],
		[1, 'Reply', 'onSubmit={submitReply}'],
		[2, 'Edit reply', 'content={reply.body}'],
	] as const)('names card editor %i %s', (index, label, purpose) => {
		expect(cardEditors[index]).toContain(purpose);
		expect(cardEditors[index]).toContain(`ariaLabel="${label}"`);
	});
	it('leaves the new-comment composer on the default name', () => {
		const composer = source('./ItemTimeline.svelte').match(/<CommentEditor\b[\s\S]*?\/>/)?.[0];
		expect(composer).toContain('onSubmit={submitComment}');
		expect(composer).not.toContain('ariaLabel=');
	});
});
