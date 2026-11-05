// Inputs for markdown.itemPin.svelte.test.ts, shared with the Chat renderer tests.
export const ITEM_PIN_INPUTS: Record<string, string> = {
	plain: 'hello **bold** _em_ ~~del~~',
	rawHtml: '<script>alert(1)</script><img src=x onerror=alert(1)><iframe src="https://evil.example"></iframe>',
	anchors: '<a href="https://e.example/" target="_blank" rel="opener" onclick="x()">x</a> [md](https://e.example/a?b=1) [rel](/items/x)',
	badSchemes: '[a](javascript:alert(1)) [b](data:text/html,x) ![c](blob:https://x/y)',
	list: '3. three\n4. four\n\n- [ ] todo\n- [x] done',
	code: '```ts\nconst a = "<b>";\n```\n\n`inline <i>`',
	table: '| a | b |\n|---|---|\n| 1 | 2 |',
	image: '![alt](https://img.example/a.png "t")',
	attachment: '![pic](pad-attachment:11111111-2222-3333-4444-555555555555) [f](pad-attachment:11111111-2222-3333-4444-555555555555)',
	wiki: 'see [[TASK-1]] and [[other::BUG-2|shown]] and `[[TASK-3]]`',
	dataAttrs: '<span data-x="1" aria-label="y" class="c" style="color:red">s</span> <p id="i" title="t">p</p>'
};
