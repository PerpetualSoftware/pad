// The playbook editor's identity gate (TASK-3097): its async units, tabled
// with the hash of the code each was reviewed on. `identityGateSuite` says
// what the gate refuses and why. No scanner guard covers this page, so a NEW
// handler here is checked by review alone.
//
// Every unit checks the ROUTE (the loads) where it has one, AND the identity
// captured at its entry with authStore.identityFence (BUG-3236). A sign-in as
// someone else does not change the route, so the route checks alone could not
// see it.
import { identityGateSuite } from '../../../../../test/identityGateSuite';

identityGateSuite({
	surface: 'playbooks/[slug]',
	source: new URL('./+page.svelte', import.meta.url),
	table: {
		asyncFunctions: {
			loadItem: { reviewed: '1d65e4a33dc3', why: 'workspace, ref AND the entry identity fence after the fetch, on both arms and in the finally' },
			loadPlaybooks: { reviewed: '6825e2ad3d9b', why: 'workspace AND the entry identity fence after the fetch, on both arms' },
			loadCollection: { reviewed: '5e49f16c7659', why: 'workspace AND the entry identity fence after the fetch, on both arms' },
			save: { reviewed: '9a71c395f37e', why: 'user-initiated save; the entry identity fence before the dialog, after its answer (so the overwrite re-send never goes under another identity), before the success report and navigation, on the failure report, and on the finally that clears saving' },
			handleExport: { reviewed: 'bd5b6bebfde2', why: 'user-initiated export; the entry identity fence before either toast and on the finally that clears exporting' },
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
			old: "\t\t\tconst coll = await api.collections.get(ws, 'playbooks');\n\t\t\tif (ws !== wsSlug || !isSameIdentity()) return;\n",
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
			old: "\t\t\tconst list = await api.items.listByCollection(ws, 'playbooks', {});\n\t\t\tif (ws !== wsSlug || !isSameIdentity()) return;\n",
			new: "\t\t\tconst list = await api.items.listByCollection(ws, 'playbooks', {});\n\t\t\tif ((ws !== wsSlug || !isSameIdentity()) && list === null) return;\n",
			names: 'loadPlaybooks()',
		},
	],
	control: { old: '<script lang="ts">\n', new: '<script lang="ts">\n\tconst controlUnused = 0;\n' },
});
