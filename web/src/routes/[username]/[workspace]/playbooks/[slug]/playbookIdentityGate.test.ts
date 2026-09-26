// The playbook editor's identity gate (TASK-3097): its async units, tabled
// with the hash of the code each was reviewed on. `identityGateSuite` says
// what the gate refuses and why. No scanner guard covers this page, so a NEW
// handler here is checked by review alone.
//
// None of these units checks the signed-in identity. The loads check the ROUTE
// (workspace, and the ref for loadItem), which a sign-in as someone else does
// not change. The advisory list on TASK-3097 carries that. The table records
// what the code does. It does not claim the code is right.
import { identityGateSuite } from '../../../../../test/identityGateSuite';

identityGateSuite({
	surface: 'playbooks/[slug]',
	source: new URL('./+page.svelte', import.meta.url),
	table: {
		asyncFunctions: {
			loadItem: { reviewed: 'adf633f5560a', why: 'workspace and ref against the route after the fetch, on both arms and in the finally; no identity check' },
			loadPlaybooks: { reviewed: 'f73fbbb97fd7', why: 'workspace against the route after the fetch, on both arms; no identity check' },
			loadCollection: { reviewed: '0bf7d6d289ac', why: 'workspace against the route after the fetch, on both arms; no identity check' },
			save: { reviewed: '2d8574271b2c', why: 'user-initiated save; no check after any await, including the overwrite re-send after the pending-edits dialog' },
			handleExport: { reviewed: 'c22f48fa6092', why: 'user-initiated export; writes only its own toast and busy flag' },
		},
		nested: [],
		markup: [],
		continuations: [],
		helpers: {},
		identifierCallbacks: [],
	},
	mutants: [
		{
			cls: 1,
			what: 'a commit between the item fetch and its route check',
			old: '\t\t\tconst loaded = await api.items.get(ws, slugOrRef);\n',
			new: '\t\t\tconst loaded = await api.items.get(ws, slugOrRef);\n\t\t\ttitle = loaded.title;\n',
			names: 'loadItem()',
		},
		{
			cls: 2,
			what: 'an async object method at component level',
			old: '\tasync function loadPlaybooks(ws: string) {\n',
			new: '\tconst extra = { async run() { await Promise.resolve(); existingPlaybooks = []; } };\n\tasync function loadPlaybooks(ws: string) {\n',
			names: 'nested async function',
		},
		{
			cls: 3,
			what: "loadCollection's success arm loses its own check while its failure arm keeps one",
			old: "\t\t\tconst coll = await api.collections.get(ws, 'playbooks');\n\t\t\tif (ws !== wsSlug) return;\n",
			new: "\t\t\tconst coll = await api.collections.get(ws, 'playbooks');\n",
			names: 'loadCollection()',
		},
		{
			cls: 4,
			what: "loadItem's route check keeps its shape and compares a value with itself",
			old: '\t\t\tconst loaded = await api.items.get(ws, slugOrRef);\n\t\t\t// ',
			new: '\t\t\tconst loaded = await api.items.get(ws, slugOrRef);\n\t\t\tws = wsSlug;\n\t\t\t// ',
			names: 'loadItem()',
		},
		{
			cls: 5,
			what: "loadPlaybooks' success return is made conditional on something that never holds",
			old: "\t\t\tconst list = await api.items.listByCollection(ws, 'playbooks', {});\n\t\t\tif (ws !== wsSlug) return;\n",
			new: "\t\t\tconst list = await api.items.listByCollection(ws, 'playbooks', {});\n\t\t\tif (ws !== wsSlug && list === null) return;\n",
			names: 'loadPlaybooks()',
		},
	],
	control: { old: '<script lang="ts">\n', new: '<script lang="ts">\n\tconst controlUnused = 0;\n' },
});
