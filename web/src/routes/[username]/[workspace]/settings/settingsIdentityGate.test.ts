// The workspace settings page's identity gate (TASK-3097): its async units and
// deferred callbacks, tabled with the hash of the code each was reviewed on.
// `identityGateSuite` says what the gate refuses and why.
// `settingsIdentityFence.test.ts` beside this keeps the site pins the gate
// does not cover, and `settingsIdentityFence.svelte.test.ts` owns the
// semantics on a mount.
import { identityGateSuite } from '../../../../test/identityGateSuite';

const ENTRY = 'identityHeld(epochAtEntry) after each await, on both arms';
const TIMER = 'resets the status 2s later, only if identityHeld(epochAtEntry) still holds';

identityGateSuite({
	surface: 'settings',
	source: new URL('./+page.svelte', import.meta.url),
	table: {
		asyncFunctions: {
			refreshCollections: { reviewed: '8eae0856214c', why: 'collectionsGen and the workspace, then identityHeld(epochAtEntry), before the commit; a failure commits nothing' },
			load: {
				reviewed: '3ed7f506bd0f',
				why: 'loadGen after each await (collectionsGen too for the collections write). It has no identity check, and it re-stamps identityEpochAtLoad BEFORE its awaits, where roles and library re-stamp after the data lands. The advisory list on TASK-3097 carries that. Re-reviewed for BUG-3260: it no longer writes the name or context (an effect seeds them from the settled store), so it commits strictly less than before. Re-reviewed for TASK-3413 U9c: the members apps list is written beside members, behind the same loadGen check',
			},
			saveName: { reviewed: '873c783bf6cb', why: `${ENTRY}; the finally clears the button's own busy flag unfenced, on purpose` },
			saveContext: { reviewed: 'c7599b4b7ff4', why: `${ENTRY}, including between the update and the shared-store setCurrent; the finally clears its own busy flag` },
			handleCollectionCreated: { reviewed: 'd79a036401d8', why: 'identityHeld(epochAtEntry) after refreshCollections, which fences its own commit' },
			handleCollectionUpdated: { reviewed: '2f678b54cc5f', why: 'identityHeld(epochAtEntry) after refreshCollections, which fences its own commit' },
			handleInvite: {
				reviewed: '21d6ac84d817',
				why: `${ENTRY}, before the clipboard write as well as the page commits; the finally clears its own busy flag. Re-reviewed for TASK-3413 U9c: the members apps list is written beside members, after the second await's check`,
			},
			handleRemoveMember: { reviewed: '3dc39d4d09be', why: `${ENTRY}. Re-reviewed for TASK-2190: the failure toast reads the server's reason through memberChangeFailure, after the check` },
			handleCancelInvitation: { reviewed: 'fba3090b1e99', why: ENTRY },
			handleChangeRole: { reviewed: '13b16ee68d27', why: `${ENTRY}. Re-reviewed for TASK-2190: the self-demotion confirm and its revert run before any await; the failure arm reverts the select and toasts the server's reason only after the check` },
			toggleAccessPanel: { reviewed: '5608048fa64f', why: `${ENTRY}; the finally clears the panel's loading flag` },
			saveCollectionAccess: { reviewed: 'f81531fe3ad4', why: `${ENTRY}, the revert included; the finally clears its own busy flag` },
			handleDeleteWorkspace: {
				reviewed: 'f9b3d9cf8e03',
				why: 'identityHeld(epochAtDelete) after the delete on both arms; the Undo callback it hands the global toast compares the epoch at click time against the one captured at delete time',
			},
			undoDeleteWorkspace: { reviewed: 'd9f84d9f668c', why: "identityHeld(epochAtDelete), the caller's delete-time epoch, after the restore on both arms" },
		},
		nested: [],
		markup: [
			{ body: /copyToClipboard\(url\)/, why: 'Copy invite link: pageIdentityHeld() before reading the invitation and again before the toast', reviewed: 'e9571b58f38c' },
		],
		continuations: [
			{ call: /^setTimeout\($/, body: /nameStatus = 'idle'/, in: 'saveName', why: `saveName status timer: ${TIMER}`, reviewed: '2a72a8c678c4' },
			{ call: /^setTimeout\($/, body: /contextStatus = 'idle'/, in: 'saveContext', why: `saveContext status timer: ${TIMER}`, reviewed: '8b862e3ec852' },
		],
		helpers: {
			captureIdentity: '7f6903e09e84',
			formatContextEditor: 'eea9d8321434',
			identityHeld: 'e4fd3989a707',
			memberChangeFailure: '1836a1c54677',
			pageIdentityHeld: 'a7bbe5de91a4',
			stripContextFromSettings: 'd5d9344d94a3',
		},
		identifierCallbacks: [],
	},
	mutants: [
		{
			cls: 1,
			what: 'a commit between the role update and its check',
			old: '\t\t\tawait api.members.updateRole(wsSlug, userId, newRole);\n',
			new: '\t\t\tawait api.members.updateRole(wsSlug, userId, newRole);\n\t\t\tmembers = [];\n',
			names: 'handleChangeRole()',
		},
		{
			cls: 2,
			what: 'an async object method at component level',
			old: '\tasync function saveName() {\n',
			new: '\tconst extra = { async run() { await Promise.resolve(); members = []; } };\n\tasync function saveName() {\n',
			names: 'nested async function',
		},
		{
			cls: 3,
			what: "saveContext's check between its two awaits is dropped while the one after the second stays",
			old: '\t\t\tif (!identityHeld(epochAtEntry)) return;\n\t\t\tawait workspaceStore.setCurrent(updated);\n',
			new: '\t\t\tawait workspaceStore.setCurrent(updated);\n',
			names: 'saveContext()',
		},
		{
			cls: 3,
			what: "the Undo callback loses its click-time check while the handler's own checks stay",
			old: '\t\t\t\t\tif (authStore.identityEpoch !== epochAtDelete) return;\n',
			new: '',
			names: 'handleDeleteWorkspace()',
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
			what: "the Copy invite link handler's second check is made conditional on something that never holds",
			old: "if (!pageIdentityHeld()) return; toastStore.show(ok ? 'Link copied!'",
			new: "if (!pageIdentityHeld() && url === '') return; toastStore.show(ok ? 'Link copied!'",
			names: 'in markup',
		},
	],
	control: { old: '<script lang="ts">\n', new: '<script lang="ts">\n\tconst controlUnused = 0;\n' },
});
