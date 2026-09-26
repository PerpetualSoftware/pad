// The starred page's identity gate (TASK-3097): its one async unit, tabled with
// the hash of the code it was reviewed on. `identityGateSuite` says what the
// gate refuses and why.
import { identityGateSuite } from '../../../../test/identityGateSuite';

identityGateSuite({
	surface: 'starred',
	source: new URL('./+page.svelte', import.meta.url),
	table: {
		asyncFunctions: {
			loadStarred: {
				reviewed: 'f72c34c5d1c7',
				why: 'seq against loadSeq AND the entry identity fence (authStore.identityFence) before committing; the finally clears loading only under both',
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
			what: 'a commit between the await and the check',
			old: '\t\t\tif (seq !== loadSeq || !isSameIdentity()) return;\n',
			new: '\t\t\tcollections = [];\n\t\t\tif (seq !== loadSeq || !isSameIdentity()) return;\n',
			names: 'loadStarred()',
		},
		{
			cls: 2,
			what: 'a nested async arrow inside the load',
			old: '\t\tconst isSameIdentity = authStore.identityFence();\n',
			new: '\t\tconst isSameIdentity = authStore.identityFence();\n\t\tconst later = async () => { await Promise.resolve(); fetchedItems = []; };\n\t\tvoid later;\n',
			names: 'nested async function',
		},
		{
			cls: 3,
			what: 'the identity half of the success check is dropped, the sequence half kept',
			old: '\t\t\tif (seq !== loadSeq || !isSameIdentity()) return;\n',
			new: '\t\t\tif (seq !== loadSeq) return;\n',
			names: 'loadStarred()',
		},
		{
			cls: 4,
			what: 'the identity fence keeps its name and stops comparing anything',
			old: '\t\tconst isSameIdentity = authStore.identityFence();\n',
			new: '\t\tconst isSameIdentity = () => true;\n',
			names: 'loadStarred()',
		},
		{
			cls: 5,
			what: "the success check's return is made conditional on something that never holds",
			old: '\t\t\tif (seq !== loadSeq || !isSameIdentity()) return;\n',
			new: '\t\t\tif ((seq !== loadSeq || !isSameIdentity()) && colls === null) return;\n',
			names: 'loadStarred()',
		},
	],
	control: { old: '<script lang="ts">\n', new: '<script lang="ts">\n\tconst controlUnused = 0;\n' },
});
