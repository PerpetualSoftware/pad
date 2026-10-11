// markdown-it ships no types for its CommonMark HTML-block tag list, which
// htmlProse.ts reads (BUG-3557).
declare module 'markdown-it/lib/common/html_blocks.mjs' {
	const names: string[];
	export default names;
}
