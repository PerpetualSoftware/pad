// The starred page's identity gate (TASK-3097): its one async unit, tabled with
// the hash of the code it was reviewed on. `identityGateSuite` says what the
// gate refuses and why. No scanner guard covers this page, so a NEW handler
// here is checked by review alone.
import { identityGateSuite } from '../../../../test/identityGateSuite';

identityGateSuite({
	surface: 'starred',
	source: new URL('./+page.svelte', import.meta.url),
	table: {
		asyncFunctions: {
			// TASK-2231: the page's own items fetch (loadStarred) is gone; it reads
			// the starred store and the local index. Its one async unit now only
			// records whether the collection list failed.
			ensurePageCollections: {
				reviewed: '5903a73c72d1',
				why: 'writes collectionsError only when the workspace it asked for is still the page\'s AND the entry identity fence (authStore.identityFence) holds, on both the success and the failure arm',
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
			old: '\t\t\tawait collectionStore.ensureCollections(ws);\n\t\t\tif (ws !== wsSlug || !isSameIdentity()) return;\n',
			new: '\t\t\tawait collectionStore.ensureCollections(ws);\n\t\t\tcollectionsError = null;\n\t\t\tif (ws !== wsSlug || !isSameIdentity()) return;\n',
			names: 'ensurePageCollections()',
		},
		{
			cls: 2,
			what: 'a nested async arrow inside the load',
			old: '\t\tconst isSameIdentity = authStore.identityFence();\n',
			new: '\t\tconst isSameIdentity = authStore.identityFence();\n\t\tconst later = async () => { await Promise.resolve(); collectionsError = null; };\n\t\tvoid later;\n',
			names: 'nested async function',
		},
		{
			cls: 3,
			what: 'the identity half of the failure check is dropped, the workspace half kept',
			old: '\t\t} catch (err) {\n\t\t\tif (ws !== wsSlug || !isSameIdentity()) return;\n',
			new: '\t\t} catch (err) {\n\t\t\tif (ws !== wsSlug) return;\n',
			names: 'ensurePageCollections()',
		},
		{
			cls: 4,
			what: 'the identity fence keeps its name and stops comparing anything',
			old: '\t\tconst isSameIdentity = authStore.identityFence();\n',
			new: '\t\tconst isSameIdentity = () => true;\n',
			names: 'ensurePageCollections()',
		},
		{
			cls: 5,
			what: "the success check's return is made conditional on something that never holds",
			old: '\t\t\tawait collectionStore.ensureCollections(ws);\n\t\t\tif (ws !== wsSlug || !isSameIdentity()) return;\n',
			new: '\t\t\tawait collectionStore.ensureCollections(ws);\n\t\t\tif ((ws !== wsSlug || !isSameIdentity()) && ws === null) return;\n',
			names: 'ensurePageCollections()',
		},
	],
	control: { old: '<script lang="ts">\n', new: '<script lang="ts">\n\tconst controlUnused = 0;\n' },
});
