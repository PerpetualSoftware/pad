/**
 * iOS zooms into any focused field whose text is under 16px. The zoom pans the
 * page sideways and puts the editor's keyboard toolbar under the keyboard,
 * because a fixed element is placed against the layout viewport while
 * innerHeight then reports the zoomed one (BUG-4482).
 *
 * `maximum-scale=1` stops that zoom. Since iOS 10 it does not block pinch
 * zoom, so on iOS nothing else changes. Elsewhere (Android) it would block
 * pinch zoom, so it is added on iOS only.
 */

type NavLike = Pick<Navigator, 'userAgent' | 'maxTouchPoints'> & { platform?: string };

/** iPhone, iPod or iPad, including an iPad that reports itself as a Mac. */
export function isIOS(nav: NavLike): boolean {
	if (/iPhone|iPad|iPod/.test(nav.userAgent)) return true;
	return nav.platform === 'MacIntel' && nav.maxTouchPoints > 1;
}

export function stopIOSFocusZoom(doc: Document, nav: NavLike): void {
	if (!isIOS(nav)) return;
	const meta = doc.querySelector<HTMLMetaElement>('meta[name="viewport"]');
	if (!meta || /maximum-scale/.test(meta.content)) return;
	meta.content = `${meta.content}, maximum-scale=1`;
}
