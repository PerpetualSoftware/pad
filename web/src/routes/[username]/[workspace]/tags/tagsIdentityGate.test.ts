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
				reviewed: '0f72a690750a',
				why: 'seq against loadSeq AND the entry identity fence (authStore.identityFence) on both arms; the finally clears loading only under both. Same shape as starred (BUG-3236)',
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
			what: 'the identity half of the success check is dropped, the sequence half kept',
			old: '\t\t\tif (seq !== loadSeq || !isSameIdentity()) return;\n\t\t\ttags = result;\n',
			new: '\t\t\tif (seq !== loadSeq) return;\n\t\t\ttags = result;\n',
			names: 'loadTags()',
		},
		{
			cls: 4,
			what: 'the identity fence keeps its name and stops comparing anything',
			old: '\t\tconst isSameIdentity = authStore.identityFence();\n',
			new: '\t\tconst isSameIdentity = () => true;\n',
			names: 'loadTags()',
		},
		{
			cls: 5,
			what: "the failure arm's return is made conditional on something that never holds",
			old: '\t\t} catch {\n\t\t\tif (seq !== loadSeq || !isSameIdentity()) return;\n',
			new: '\t\t} catch {\n\t\t\tif ((seq !== loadSeq || !isSameIdentity()) && tags.length < 0) return;\n',
			names: 'loadTags()',
		},
	],
	control: { old: '<script lang="ts">\n', new: '<script lang="ts">\n\tconst controlUnused = 0;\n' },
});
