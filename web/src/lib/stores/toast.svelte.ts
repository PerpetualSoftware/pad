export interface Toast {
	id: string;
	message: string;
	type: 'success' | 'error' | 'info';
	duration: number;
}

const MAX_TOASTS = 5;
const DEFAULT_DURATION = 3000;

let toasts = $state<Toast[]>([]);
const timers = new Map<string, ReturnType<typeof setTimeout>>();

function generateId(): string {
	return Date.now().toString(36) + Math.random().toString(36).slice(2, 7);
}

function show(message: string, type: Toast['type'] = 'info', duration: number = DEFAULT_DURATION): string {
	const id = generateId();
	const toast: Toast = { id, message, type, duration };

	toasts.push(toast);

	// Drop oldest if exceeded max
	while (toasts.length > MAX_TOASTS) {
		const oldest = toasts.shift();
		if (oldest) {
			clearTimerFor(oldest.id);
		}
	}

	// Auto-dismiss after duration
	const timer = setTimeout(() => {
		dismiss(id);
	}, duration);
	timers.set(id, timer);

	return id;
}

function dismiss(id: string): void {
	clearTimerFor(id);
	const idx = toasts.findIndex((t) => t.id === id);
	if (idx !== -1) {
		toasts.splice(idx, 1);
	}
}

function clearTimerFor(id: string): void {
	const timer = timers.get(id);
	if (timer) {
		clearTimeout(timer);
		timers.delete(id);
	}
}

export const toastStore = {
	get toasts(): Toast[] {
		return toasts;
	},
	show,
	dismiss
};
