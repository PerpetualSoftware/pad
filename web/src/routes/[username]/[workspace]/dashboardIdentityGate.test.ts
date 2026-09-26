// The workspace dashboard's identity gate (TASK-3097): its async units, tabled
// with the hash of the code each was reviewed on. `identityGateSuite` says
// what the gate refuses and why. `dashboardIdentityFence.test.ts` beside this
// keeps the site pins the gate does not cover (the sync listener, the
// scroll-restoration wiring), and `dashboardIdentityFence.svelte.test.ts` owns
// the semantics on a mount.
import { identityGateSuite } from '../../../test/identityGateSuite';

identityGateSuite({
	surface: '[workspace] dashboard',
	source: new URL('./+page.svelte', import.meta.url),
	table: {
		asyncFunctions: {
			load: {
				reviewed: '46a055c105cd',
				why: 'identityHeld(epochAtEntry) before the fetches; dashLoadSeq and identityHeld before every commit on both arms; the finally clears loading on the sequence alone, deliberately (#1378)',
			},
		},
		nested: [],
		markup: [],
		continuations: [
			{
				call: /^setInterval\($/,
				body: /load\(wsSlug, true\)/,
				in: 'onMount(…)',
				why: 'the 30s poll: calls load, which captures the identity at its own entry',
				reviewed: 'e407931b5d02',
			},
		],
		helpers: {
			captureIdentity: '7f6903e09e84',
			identityHeld: 'e4fd3989a707',
		},
		identifierCallbacks: [],
	},
	mutants: [
		{
			cls: 1,
			what: 'a commit between the workspace switch and the identity check',
			old: '\t\t\tawait workspaceStore.setCurrent(slug);\n',
			new: '\t\t\tawait workspaceStore.setCurrent(slug);\n\t\t\tdashError = null;\n',
			names: 'load()',
		},
		{
			cls: 2,
			what: 'the poll callback becomes async',
			old: '\t\tpollTimer = setInterval(() => {\n',
			new: '\t\tpollTimer = setInterval(async () => {\n',
			names: 'the 30s poll',
		},
		{
			cls: 3,
			what: "the catch arm's check is dropped while the success arm keeps its own",
			old: '\t\t\t// commit, and it would be read by whoever is signed in now.\n\t\t\tif (seq !== dashLoadSeq || !identityHeld(epochAtEntry)) return;\n',
			new: '\t\t\t// commit, and it would be read by whoever is signed in now.\n',
			names: 'load()',
		},
		{
			cls: 4,
			what: 'identityHeld keeps its name and stops comparing',
			old: '\t\treturn authStore.identityEpoch === captured;\n',
			new: '\t\treturn authStore.identityEpoch === authStore.identityEpoch;\n',
			names: 'helper identityHeld()',
		},
		{
			cls: 5,
			what: "the success check's return is made conditional on something that never holds",
			old: '\t\t\tif (seq !== dashLoadSeq || !identityHeld(epochAtEntry)) return;\n\t\t\tdashboard = dash;\n',
			new: '\t\t\tif ((seq !== dashLoadSeq || !identityHeld(epochAtEntry)) && dash === null) return;\n\t\t\tdashboard = dash;\n',
			names: 'load()',
		},
	],
	control: { old: '<script lang="ts">\n', new: '<script lang="ts">\n\tconst controlUnused = 0;\n' },
});
