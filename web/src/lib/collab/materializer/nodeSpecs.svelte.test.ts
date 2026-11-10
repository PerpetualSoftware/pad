import { expect, it } from 'vitest';
import { createHeadlessEditor } from './headless';

it('preserves every node spec from the frozen schema at 4c45ec71a', () => {
	const ed = createHeadlessEditor();
	try {
		const nodes: Record<string, unknown> = {};
		ed.schema.spec.nodes.forEach((name, spec) => { nodes[name] = spec; });
		// Include parse/render functions as well as all persisted shape fields.
		const dump = JSON.parse(JSON.stringify(nodes, (_key, value) => typeof value === 'function' ? value.toString() : value));
		expect(dump).toMatchSnapshot();
	} finally { ed.destroy(); }
});
