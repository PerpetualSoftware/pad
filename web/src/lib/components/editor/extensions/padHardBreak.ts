import HardBreak from '@tiptap/extension-hard-break';
import type { Node } from '@tiptap/pm/model';
import type MarkdownIt from 'markdown-it';

let flatSoftBreaks = false;

/** Parse the pre-existing flat representation without changing the live document. */
export function withFlatSoftBreaks<T>(fn: () => T): T {
	const previous = flatSoftBreaks;
	flatSoftBreaks = true;
	try {
		return fn();
	} finally {
		flatSoftBreaks = previous;
	}
}

const registered = new WeakSet<MarkdownIt>();

type BreakState = { inTable?: boolean; write(value: string): void };

/** Use the existing hardBreak node for body newlines, without changing its spec. */
export const PadHardBreak = HardBreak.extend({
	addStorage() {
		return {
			markdown: {
				serialize(state: BreakState, node: Node, parent: Node, index: number) {
					// Keep tiptap-markdown's omission of a trailing run of breaks.
					for (let i = index + 1; i < parent.childCount; i++) {
						if (parent.child(i).type === node.type) continue;
						const previous = index > 0 ? parent.child(index - 1) : null;
						const plain = parent.type.name !== 'heading' && previous &&
							previous.type !== node.type && (!previous.isText || /\S/.test(previous.text ?? ''));
						state.write(state.inTable ? '<br>' : plain ? '\n' : '\\\n');
						return;
					}
				},
				parse: {
					setup(md: MarkdownIt) {
						if (registered.has(md)) return;
						registered.add(md);
						md.renderer.rules.softbreak = () => flatSoftBreaks ? '\n' : '<br>';
						// A multiline setext heading remains one heading, as before.
						md.core.ruler.after('inline', 'pad_heading_softbreaks', (state) => {
							let heading = false;
							for (const token of state.tokens) {
								if (token.type === 'heading_open') heading = true;
								if (token.type === 'heading_close') heading = false;
								if (!heading || !token.children) continue;
								for (const child of token.children) {
									if (child.type !== 'softbreak') continue;
									child.type = 'text';
									child.content = ' ';
								}
							}
						});
					},
				},
			},
		};
	},
});
