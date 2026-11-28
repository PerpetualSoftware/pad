<script lang="ts">
	// Stands in for HomePaneHost in DesktopHome wiring tests: records open and dismiss calls.
	import type { HomePaneTarget } from '../homePane';
	import { homePaneHostStubCalls } from './homePaneHostStubCalls';

	export function open(target: HomePaneTarget) {
		homePaneHostStubCalls.open.push(target);
	}
	export function dismissOnBackgroundClick(e: MouseEvent) {
		homePaneHostStubCalls.dismiss.push(e);
	}
</script>
