// The library page's identity gate (TASK-3097): its async units and deferred
// callbacks, tabled with the hash of the code each was reviewed on.
// `identityGateSuite` says what the gate refuses and why.
// `libraryIdentityFence.test.ts` beside this keeps the site pins the gate does
// not cover, and `libraryIdentityFence.svelte.test.ts` owns the semantics on a
// mount.
import { identityGateSuite } from '../../../../test/identityGateSuite';

const TIMER_WHY = 'clears the toast 3s later, only if identityHeld(epochAtEntry) still holds';

identityGateSuite({
	surface: 'library',
	source: new URL('./+page.svelte', import.meta.url),
	table: {
		asyncFunctions: {
			loadData: {
				reviewed: '1a49c520fffd',
				why: 'identityHeld(epochAtEntry) then loadGen on both arms before any commit; re-stamps identityEpochAtLoad only after the data it vouches for; the finally clears loading on loadGen alone, deliberately (#1378)',
			},
			activateConvention: {
				reviewed: '648517945a1b',
				why: 'pageIdentityHeld() before the write (the choice came from the loaded list), identityHeld(epochAtEntry) after it on both arms and in the finally',
			},
			activatePlaybook: {
				reviewed: '5f104126623d',
				why: 'pageIdentityHeld() before the write (the choice came from the loaded list), identityHeld(epochAtEntry) after it on both arms and in the finally',
			},
		},
		nested: [],
		markup: [],
		continuations: [
			{ call: /\(ws, convSlug\)\.catch\($/, body: /./, why: 'loadData conventions fetch: a failure reads as none; commits nothing', reviewed: 'cb8bf2ca350f' },
			{ call: /\(ws, pbSlug\)\.catch\($/, body: /./, why: 'loadData playbooks fetch: a failure reads as none; commits nothing', reviewed: 'cb8bf2ca350f' },
			{ call: /builtins\.list\(ws\)\.catch\($/, body: /./, why: 'loadData built-ins fetch (TASK-3462 U3b): a failure, or a server without the listing, reads as none; commits nothing', reviewed: 'eb80d2613170' },
			{ call: /^setTimeout\($/, body: /identityHeld\(epochAtEntry\)/, in: 'activateConvention', count: 2, why: `activateConvention toast timer, one per arm: ${TIMER_WHY}`, reviewed: '14cf7fba65c1' },
			{ call: /^setTimeout\($/, body: /identityHeld\(epochAtEntry\)/, in: 'activatePlaybook', count: 2, why: `activatePlaybook toast timer, one per arm: ${TIMER_WHY}`, reviewed: 'e66e8d41273e' },
		],
		helpers: {
			captureIdentity: '7f6903e09e84',
			identityHeld: 'e4fd3989a707',
			pageIdentityHeld: 'd6d5caf46ddb',
		},
		identifierCallbacks: [],
	},
	mutants: [
		{
			cls: 1,
			what: 'a commit between the activation request and its check',
			old: '\t\t\tawait api.library.activate(wsSlug, convention);\n',
			new: '\t\t\tawait api.library.activate(wsSlug, convention);\n\t\t\ttoast = null;\n',
			names: 'activateConvention()',
		},
		{
			cls: 2,
			what: 'an async object method at component level',
			old: '\tasync function loadData(ws: string, convSlug: string = conventionsSlug, pbSlug: string = playbooksSlug) {\n',
			new: '\tconst extra = { async run() { await Promise.resolve(); toast = null; } };\n\tasync function loadData(ws: string, convSlug: string = conventionsSlug, pbSlug: string = playbooksSlug) {\n',
			names: 'nested async function',
		},
		{
			cls: 3,
			what: "activatePlaybook's success arm loses its check while its failure arm keeps one",
			old: '\t\t\tawait api.library.activatePlaybook(wsSlug, playbook);\n\t\t\tif (!identityHeld(epochAtEntry)) return;\n',
			new: '\t\t\tawait api.library.activatePlaybook(wsSlug, playbook);\n',
			names: 'activatePlaybook()',
		},
		{
			cls: 3,
			what: "one of activatePlaybook's two toast timers loses its check while the other keeps it",
			old: '\t\t\ttoast = `Failed to activate: ${playbook.title}`;\n\t\t\tsetTimeout(() => {\n\t\t\t\tif (!identityHeld(epochAtEntry)) return;\n',
			new: '\t\t\ttoast = `Failed to activate: ${playbook.title}`;\n\t\t\tsetTimeout(() => {\n',
			names: 'activatePlaybook toast timer',
		},
		{
			cls: 4,
			what: 'pageIdentityHeld keeps its name and stops comparing',
			old: '\t\treturn authStore.identityEpoch === identityEpochAtLoad;\n',
			new: '\t\treturn authStore.identityEpoch === authStore.identityEpoch;\n',
			names: 'helper pageIdentityHeld()',
		},
		{
			cls: 5,
			what: "loadData's generation return is made conditional on something that never holds",
			old: '\t\t\tif (myLoad !== loadGen) return;\n\t\t\tcategories = libraryRes.categories;\n',
			new: '\t\t\tif (myLoad !== loadGen && libraryRes === null) return;\n\t\t\tcategories = libraryRes.categories;\n',
			names: 'loadData()',
		},
	],
	control: { old: '<script lang="ts">\n', new: '<script lang="ts">\n\tconst controlUnused = 0;\n' },
});
