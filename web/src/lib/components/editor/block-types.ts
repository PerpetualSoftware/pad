/**
 * Shared block type definitions used by both the slash command menu
 * and the "Turn into" context menu.
 */

export interface BlockType {
	id: string;
	icon: string;
	label: string;
	description: string;
	/**
	 * Search aliases for the slash-command menu. The filter matches the query
	 * against the label, description, id, and each keyword. Use this for
	 * common abbreviations the label/description wouldn't otherwise cover
	 * (e.g. `h2` for "Heading 2", `hr` for "Divider", `todo` for "Checklist").
	 */
	keywords?: string[];
	/** Only available via slash command (insert), not turn-into (convert) */
	insertOnly?: boolean;
	/** Only available via turn-into (convert), not slash command (insert) */
	convertOnly?: boolean;
}

export const BLOCK_TYPES: BlockType[] = [
	{ id: 'paragraph', icon: 'Aa', label: 'Text', description: 'Plain text', convertOnly: true },
	{ id: 'heading1', icon: 'H1', label: 'Heading 1', description: 'Large heading', keywords: ['h1'] },
	{ id: 'heading2', icon: 'H2', label: 'Heading 2', description: 'Medium heading', keywords: ['h2'] },
	{ id: 'heading3', icon: 'H3', label: 'Heading 3', description: 'Small heading', keywords: ['h3'] },
	{ id: 'bulletList', icon: '•', label: 'Bullet List', description: 'Unordered list', keywords: ['ul', 'bullets', 'unordered'] },
	{ id: 'orderedList', icon: '1.', label: 'Numbered List', description: 'Ordered list', keywords: ['ol', 'numbered', 'ordered'] },
	{ id: 'taskList', icon: '☐', label: 'Checklist', description: 'Task list', keywords: ['todo', 'task', 'checkbox', 'check'] },
	{ id: 'codeBlock', icon: '<>', label: 'Code Block', description: 'Fenced code block', keywords: ['code', 'pre'] },
	{ id: 'htmlBlock', icon: 'HTML', label: 'HTML Block', description: 'Sanitized HTML embed (live preview)', insertOnly: true, keywords: ['html', 'embed'] },
	{ id: 'blockquote', icon: '❝', label: 'Blockquote', description: 'Quote block', keywords: ['quote', 'bq'] },
	{ id: 'horizontalRule', icon: '——', label: 'Divider', description: 'Horizontal rule', insertOnly: true, keywords: ['hr', 'rule', 'separator'] },
	{ id: 'table', icon: '⊞', label: 'Table', description: '3×3 table', insertOnly: true, keywords: ['tbl', 'grid'] },
];

/** Block types available in the slash command menu (excludes convert-only types like "Text") */
export const SLASH_ITEMS = BLOCK_TYPES.filter((b) => !b.convertOnly);

/** Block types available in the "Turn into" menu (excludes insert-only types like Divider and Table) */
export const TURN_INTO_ITEMS = BLOCK_TYPES.filter((b) => !b.insertOnly);
