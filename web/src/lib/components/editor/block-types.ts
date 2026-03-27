/**
 * Shared block type definitions used by both the slash command menu
 * and the "Turn into" context menu.
 */

export interface BlockType {
	id: string;
	icon: string;
	label: string;
	description: string;
	/** Only available via slash command (insert), not turn-into (convert) */
	insertOnly?: boolean;
	/** Only available via turn-into (convert), not slash command (insert) */
	convertOnly?: boolean;
}

export const BLOCK_TYPES: BlockType[] = [
	{ id: 'paragraph', icon: 'Aa', label: 'Text', description: 'Plain text', convertOnly: true },
	{ id: 'heading1', icon: 'H1', label: 'Heading 1', description: 'Large heading' },
	{ id: 'heading2', icon: 'H2', label: 'Heading 2', description: 'Medium heading' },
	{ id: 'heading3', icon: 'H3', label: 'Heading 3', description: 'Small heading' },
	{ id: 'bulletList', icon: '•', label: 'Bullet List', description: 'Unordered list' },
	{ id: 'orderedList', icon: '1.', label: 'Numbered List', description: 'Ordered list' },
	{ id: 'taskList', icon: '☐', label: 'Checklist', description: 'Task list' },
	{ id: 'codeBlock', icon: '<>', label: 'Code Block', description: 'Fenced code block' },
	{ id: 'blockquote', icon: '❝', label: 'Blockquote', description: 'Quote block' },
	{ id: 'horizontalRule', icon: '——', label: 'Divider', description: 'Horizontal rule', insertOnly: true },
	{ id: 'table', icon: '⊞', label: 'Table', description: '3×3 table', insertOnly: true },
];

/** Block types available in the slash command menu (excludes convert-only types like "Text") */
export const SLASH_ITEMS = BLOCK_TYPES.filter((b) => !b.convertOnly);

/** Block types available in the "Turn into" menu (excludes insert-only types like Divider and Table) */
export const TURN_INTO_ITEMS = BLOCK_TYPES.filter((b) => !b.insertOnly);
