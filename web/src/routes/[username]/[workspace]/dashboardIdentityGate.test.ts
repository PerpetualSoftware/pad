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
				reviewed: '0d1d88d24e94',
				why: 'setCurrent and both fetches issued together at entry, under the identity captured there (TASK-2229); dashLoadSeq and identityHeld before every commit on both arms; the finally clears loading on the sequence alone, deliberately (#1378)',
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
				reviewed: 'cc587e5cf2b5',
			},
			{
				call: /^setTimeout\($/,
				body: /load\(wsSlug, true\)/,
				in: 'sseService.onItemEvent(…)',
				why: 'the live-onboarding reload (BUG-3447): a debounce an item or collection event re-arms while the workspace needs onboarding; its body only calls load, which captures the identity at its own entry, and only for the workspace it was armed in (codex r1)',
				reviewed: '223f5fffc235',
			},
		],
		helpers: {
			captureIdentity: '7f6903e09e84',
			identityHeld: 'e4fd3989a707',
			pollTickWanted: 'c7b2d559cec3',
		},
		identifierCallbacks: [],
	},
	mutants: [
		{
			cls: 1,
			what: 'a commit between the answers and the identity check',
			old: '\t\t\t\tapi.collections.list(slug)\n\t\t\t]);\n',
			new: '\t\t\t\tapi.collections.list(slug)\n\t\t\t]);\n\t\t\tdashError = null;\n',
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
