// The tags page's identity gate (TASK-3097): its one async unit, tabled with
// the hash of the code it was reviewed on. `identityGateSuite` says what the
// gate refuses and why. No scanner guard covers this page, so a NEW handler
// here is checked by review alone.
import { identityGateSuite } from '../../../../test/identityGateSuite';

identityGateSuite({
	surface: 'tags',
	source: new URL('./+page.svelte', import.meta.url),
	table: {
		asyncFunctions: {
			loadTags: {
				reviewed: '647f2e65370d',
				why: 'seq against loadSeq after the fetch, on both arms and in the finally. It has no identity check, unlike starred; the advisory list on TASK-3097 carries that',
			},
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
			what: 'a request issued after the await, before any check',
			old: '\t\t\tconst result = await api.tags.list(ws);\n',
			new: '\t\t\tconst result = await api.tags.list(ws);\n\t\t\tvoid api.tags.list(ws);\n',
			names: 'loadTags()',
		},
		{
			cls: 2,
			what: 'an async object method at component level',
			old: '\tasync function loadTags(ws: string) {\n',
			new: '\tconst extra = { async run() { await Promise.resolve(); tags = []; } };\n\tasync function loadTags(ws: string) {\n',
			names: 'nested async function',
		},
		{
			cls: 3,
			what: 'the success arm loses its own check',
			old: '\t\t\tif (seq !== loadSeq) return;\n\t\t\ttags = result;\n',
			new: '\t\t\ttags = result;\n',
			names: 'loadTags()',
		},
		{
			cls: 4,
			what: 'the capture keeps its name and stops capturing a new load',
			old: '\t\tconst seq = ++loadSeq;\n',
			new: '\t\tconst seq = loadSeq;\n',
			names: 'loadTags()',
		},
		{
			cls: 5,
			what: "the failure arm's return is made conditional on something that never holds",
			old: '\t\t} catch {\n\t\t\tif (seq !== loadSeq) return;\n',
			new: '\t\t} catch {\n\t\t\tif (seq !== loadSeq && tags.length < 0) return;\n',
			names: 'loadTags()',
		},
	],
	control: { old: '<script lang="ts">\n', new: '<script lang="ts">\n\tconst controlUnused = 0;\n' },
});
