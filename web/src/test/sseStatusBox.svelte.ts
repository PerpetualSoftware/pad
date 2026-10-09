// Test-only: a reactive stand-in for sseService.status (TASK-2201 tests).
export function createStatusBox() {
	let status = $state('disconnected');
	return {
		get status() {
			return status;
		},
		set status(v: string) {
			status = v;
		}
	};
}
