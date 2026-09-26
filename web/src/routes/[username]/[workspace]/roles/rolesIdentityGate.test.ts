// The roles board's identity gate (TASK-3097): its async units and deferred
// callbacks, tabled with the hash of the code each was reviewed on.
// `identityGateSuite` says what the gate refuses and why.
// `rolesIdentityFence.test.ts` beside this keeps the site pins the gate does
// not cover, and `rolesIdentityFence.svelte.test.ts` owns the semantics on a
// mount.
import { identityGateSuite } from '../../../../test/identityGateSuite';

const PAGE_AND_ENTRY = 'pageIdentityHeld() before the write (its inputs came from the loaded board), then identityHeld(epochAtEntry) after each await';

identityGateSuite({
	surface: 'roles',
	source: new URL('./+page.svelte', import.meta.url),
	table: {
		asyncFunctions: {
			submitNewItem: { reviewed: 'd592b0b54568', why: `${PAGE_AND_ENTRY}; the finally clears newItemSaving only under the identity` },
			handleLaneDrop: { reviewed: 'd268ae879c85', why: `${PAGE_AND_ENTRY}, including before the recovery reload` },
			handleDndFinalize: {
				reviewed: '21bb6adcb7c7',
				why: `${PAGE_AND_ENTRY}; the identity check sits BEFORE the role write, because that write carries currentUserId; a lost-identity exit writes no shared interaction state`,
			},
			loadData: {
				reviewed: 'a0be40bdaf09',
				why: 'identityHeld(epochAtEntry) then loadGen on both arms before any commit; re-stamps identityEpochAtLoad only after the data it vouches for; the finally clears loading on loadGen alone, deliberately (#1378)',
			},
			saveRole: { reviewed: 'dad11e017451', why: `${PAGE_AND_ENTRY}, per branch` },
			deleteRole: { reviewed: 'ca46c5c9056d', why: PAGE_AND_ENTRY },
		},
		nested: [],
		markup: [],
		continuations: [
			{
				call: /^requestAnimationFrame\($/,
				body: /newItemTitleInput\?\.focus\(\)/,
				in: 'selectCollection',
				why: 'focuses the new-item title input the same click rendered; a DOM focus, no page state',
				reviewed: '1e51a12a4711',
			},
		],
		helpers: {
			captureIdentity: '7f6903e09e84',
			closeModal: 'e68053ba253f',
			closeNewItem: 'e727373f20df',
			identityHeld: 'e4fd3989a707',
			laneKey: '26c9b8e573fd',
			pageIdentityHeld: 'd6d5caf46ddb',
		},
		identifierCallbacks: [],
	},
	mutants: [
		{
			cls: 1,
			what: "the role write moves ahead of its identity check, so it carries a stale currentUserId",
			old: '\t\t\t\t\tif (!identityHeld(epochAtEntry)) return;\n\t\t\t\t\tawait api.items.update(wsSlug, originalItem.id, update);\n',
			new: '\t\t\t\t\tawait api.items.update(wsSlug, originalItem.id, update);\n\t\t\t\t\tif (!identityHeld(epochAtEntry)) return;\n',
			names: 'handleDndFinalize()',
		},
		{
			cls: 2,
			what: 'the rAF focus callback becomes async',
			old: '\t\trequestAnimationFrame(() => {\n',
			new: '\t\trequestAnimationFrame(async () => {\n',
			names: 'new-item title input',
		},
		{
			cls: 3,
			what: "deleteRole's success arm loses its check while its failure arm keeps one",
			old: '\t\t\tawait api.agentRoles.delete(wsSlug, editingRoleId);\n\t\t\tif (!identityHeld(epochAtEntry)) return;\n',
			new: '\t\t\tawait api.agentRoles.delete(wsSlug, editingRoleId);\n',
			names: 'deleteRole()',
		},
		{
			cls: 4,
			what: 'closeModal, reached from three handlers, starts writing state their fences do not cover',
			old: '\tfunction closeModal() {\n',
			new: '\tfunction closeModal() {\n\t\tcurrentUserId = \'\';\n',
			names: 'helper closeModal()',
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
			old: '\t\t\tif (myLoad !== loadGen) return;\n\t\t\tlanes = boardResult.lanes;\n',
			new: '\t\t\tif (myLoad !== loadGen && boardResult === null) return;\n\t\t\tlanes = boardResult.lanes;\n',
			names: 'loadData()',
		},
	],
	control: { old: '<script lang="ts">\n', new: '<script lang="ts">\n\tconst controlUnused = 0;\n' },
});
