/**
 * Block Drag Handle — Notion-style block reordering + context menu for Tiptap.
 *
 * Shows a ⠿ grip handle to the left of the current block.
 * - Tap → opens a context menu (Turn into, Duplicate, Delete)
 * - Drag → reorder the block
 *
 * Works on both desktop (mouse) and mobile (touch) via raw
 * ProseMirror transactions (no browser drag-and-drop API).
 */

import { Extension } from '@tiptap/core';
import type { Editor } from '@tiptap/core';
import { Plugin, PluginKey } from '@tiptap/pm/state';
import type { EditorView } from '@tiptap/pm/view';
import { TURN_INTO_ITEMS } from './block-types';

const LIST_CONTAINERS = new Set(['bulletList', 'orderedList', 'taskList']);
const LIST_ITEMS = new Set(['listItem', 'taskItem']);

interface BlockInfo {
	pos: number;
	node: any;
	dom: HTMLElement;
	size: number;
}

function currentBlockType(block: BlockInfo): string {
	const name = block.node.type.name;
	if (name === 'heading') return `heading${block.node.attrs.level}`;
	if (name === 'taskItem' || name === 'taskList') return 'taskList';
	if (name === 'listItem') {
		// Check parent
		const parent = block.dom.closest('ul, ol');
		if (parent?.tagName === 'OL') return 'orderedList';
		return 'bulletList';
	}
	return name;
}

function applyBlockType(editor: Editor, block: BlockInfo, targetType: string) {
	// First, place cursor inside the block so Tiptap commands target it
	editor.chain().focus(block.pos + 1).run();

	switch (targetType) {
		case 'paragraph':
			if (LIST_ITEMS.has(block.node.type.name)) {
				editor.chain().liftListItem(block.node.type.name as any).run();
			} else {
				editor.chain().setParagraph().run();
			}
			break;
		case 'heading1':
			editor.chain().setHeading({ level: 1 }).run();
			break;
		case 'heading2':
			editor.chain().setHeading({ level: 2 }).run();
			break;
		case 'heading3':
			editor.chain().setHeading({ level: 3 }).run();
			break;
		case 'bulletList':
			editor.chain().toggleBulletList().run();
			break;
		case 'orderedList':
			editor.chain().toggleOrderedList().run();
			break;
		case 'taskList':
			editor.chain().toggleTaskList().run();
			break;
		case 'codeBlock':
			editor.chain().toggleCodeBlock().run();
			break;
		case 'blockquote':
			editor.chain().toggleBlockquote().run();
			break;
	}
}

// --- Block finding helpers ---

function blockAtPos(view: EditorView, pos: number): BlockInfo | null {
	try {
		const $pos = view.state.doc.resolve(pos);
		let depth = $pos.depth;
		while (depth > 1) {
			const node = $pos.node(depth);
			if (LIST_ITEMS.has(node.type.name)) break;
			depth--;
		}
		if (depth < 1) return null;
		const startPos = $pos.before(depth);
		const node = view.state.doc.nodeAt(startPos);
		if (!node) return null;
		const dom = view.nodeDOM(startPos);
		if (!(dom instanceof HTMLElement)) return null;
		return { pos: startPos, node, dom, size: node.nodeSize };
	} catch {
		return null;
	}
}

function blockAtCoords(view: EditorView, x: number, y: number): BlockInfo | null {
	const posInfo = view.posAtCoords({ left: x, top: y });
	if (!posInfo) return null;
	return blockAtPos(view, posInfo.pos);
}

function dropPosAtY(view: EditorView, y: number, dragPos: number): number | null {
	const doc = view.state.doc;
	let best: { pos: number; dist: number } | null = null;

	function consider(pos: number, yCoord: number) {
		const dist = Math.abs(y - yCoord);
		if (!best || dist < best.dist) {
			best = { pos, dist };
		}
	}

	doc.forEach((node, offset) => {
		if (LIST_CONTAINERS.has(node.type.name)) {
			node.forEach((child, childOffset) => {
				const childPos = offset + 1 + childOffset;
				if (childPos === dragPos) return;
				const dom = view.nodeDOM(childPos);
				if (!(dom instanceof HTMLElement)) return;
				const rect = dom.getBoundingClientRect();
				consider(childPos, rect.top);
				consider(childPos + child.nodeSize, rect.bottom);
			});
			const dom = view.nodeDOM(offset);
			if (dom instanceof HTMLElement) {
				const rect = dom.getBoundingClientRect();
				consider(offset, rect.top - 4);
				consider(offset + node.nodeSize, rect.bottom + 4);
			}
		} else {
			if (offset === dragPos) return;
			const dom = view.nodeDOM(offset);
			if (!(dom instanceof HTMLElement)) return;
			const rect = dom.getBoundingClientRect();
			consider(offset, rect.top);
			consider(offset + node.nodeSize, rect.bottom);
		}
	});

	return (best as { pos: number; dist: number } | null)?.pos ?? null;
}

function executeMove(view: EditorView, fromPos: number, fromNode: any, targetPos: number) {
	const { state } = view;
	const { tr, schema } = state;

	const $target = state.doc.resolve(targetPos);
	const targetParent = $target.parent;

	let nodeToInsert = fromNode;
	if (LIST_ITEMS.has(fromNode.type.name) && targetParent.type.name === 'doc') {
		const listTypeName = fromNode.type.name === 'taskItem' ? 'taskList' : 'bulletList';
		const listType = schema.nodes[listTypeName];
		if (listType) {
			nodeToInsert = listType.create(null, [fromNode.copy(fromNode.content)]);
		}
	}

	tr.delete(fromPos, fromPos + fromNode.nodeSize);

	const checkPos = Math.min(tr.mapping.map(fromPos, -1), tr.doc.content.size);
	try {
		if (checkPos >= 0 && checkPos <= tr.doc.content.size) {
			const $check = tr.doc.resolve(checkPos);
			for (let d = $check.depth; d > 0; d--) {
				const ancestor = $check.node(d);
				if (LIST_CONTAINERS.has(ancestor.type.name) && ancestor.childCount === 0) {
					const start = $check.before(d);
					tr.delete(start, start + ancestor.nodeSize);
					break;
				}
			}
		}
	} catch {}

	let target = tr.mapping.map(targetPos);
	target = Math.max(0, Math.min(target, tr.doc.content.size));

	tr.insert(target, nodeToInsert);
	view.dispatch(tr);
}

// --- Extension ---

export const BlockDragHandle = Extension.create({
	name: 'blockDragHandle',

	addProseMirrorPlugins() {
		const pluginKey = new PluginKey('blockDragHandle');
		const tiptapEditor = this.editor;

		return [
			new Plugin({
				key: pluginKey,
				view(editorView) {
					// --- DOM: handle ---
					const handle = document.createElement('div');
					handle.className = 'block-drag-handle';
					handle.textContent = '⠿';
					handle.style.display = 'none';

					// --- DOM: drop line ---
					const dropLine = document.createElement('div');
					dropLine.className = 'block-drop-line';
					dropLine.style.display = 'none';

					// --- DOM: context menu ---
					const menu = document.createElement('div');
					menu.className = 'block-context-menu';
					menu.style.display = 'none';

					const menuBackdrop = document.createElement('div');
					menuBackdrop.className = 'block-menu-backdrop';
					menuBackdrop.style.display = 'none';

					// Build menu content
					const turnIntoLabel = document.createElement('div');
					turnIntoLabel.className = 'block-menu-label';
					turnIntoLabel.textContent = 'Turn into';
					menu.appendChild(turnIntoLabel);

					const menuItems: { el: HTMLElement; type: string }[] = [];
					for (const item of TURN_INTO_ITEMS) {
						const btn = document.createElement('button');
						btn.className = 'block-menu-item';
						btn.innerHTML = `<span class="block-menu-icon">${item.icon}</span><span>${item.label}</span>`;
						btn.dataset.type = item.id;
						menu.appendChild(btn);
						menuItems.push({ el: btn, type: item.id });
					}

					const divider = document.createElement('div');
					divider.className = 'block-menu-divider';
					menu.appendChild(divider);

					const duplicateBtn = document.createElement('button');
					duplicateBtn.className = 'block-menu-item';
					duplicateBtn.innerHTML = '<span class="block-menu-icon">⧉</span><span>Duplicate</span>';
					menu.appendChild(duplicateBtn);

					const deleteBtn = document.createElement('button');
					deleteBtn.className = 'block-menu-item block-menu-item-danger';
					deleteBtn.innerHTML = '<span class="block-menu-icon">✕</span><span>Delete</span>';
					menu.appendChild(deleteBtn);

					const wrapper = editorView.dom.parentElement!;
					wrapper.appendChild(handle);
					wrapper.appendChild(dropLine);
					document.body.appendChild(menu);
					document.body.appendChild(menuBackdrop);

					// --- State ---
					let activeBlock: BlockInfo | null = null;
					let dragging = false;
					let pendingDrag = false;
					let pendingTouchY = 0;
					let currentDropPos: number | null = null;
					let scrollRAF: number | null = null;
					let hoverMode = false;
					let userHasInteracted = false;
					let ghost: HTMLElement | null = null;
					let menuOpen = false;

					function getScrollContainer(): HTMLElement {
						return wrapper.closest('.main-content') as HTMLElement || document.documentElement;
					}

					// --- Handle positioning ---
					function positionHandle(blockDOM: HTMLElement) {
						const wrapperRect = wrapper.getBoundingClientRect();
						const blockRect = blockDOM.getBoundingClientRect();
						const handleH = handle.offsetHeight || 32;

						// Determine the vertical center of the first line of content.
						// For flex/grid containers (e.g. task list <li> with checkbox + text),
						// the block itself may be taller than one text line, so we measure
						// the first child element to align with the actual content row.
						let anchorTop = blockRect.top;
						let anchorH: number;

						const display = getComputedStyle(blockDOM).display;
						const isFlexOrGrid = display === 'flex' || display === 'inline-flex'
							|| display === 'grid' || display === 'inline-grid';
						const firstChild = isFlexOrGrid
							? blockDOM.firstElementChild as HTMLElement | null
							: null;

						if (firstChild) {
							const childRect = firstChild.getBoundingClientRect();
							anchorTop = childRect.top;
							anchorH = childRect.height;
						} else {
							const lineH = parseFloat(getComputedStyle(blockDOM).lineHeight);
							anchorH = Math.min(blockRect.height, isNaN(lineH) ? 24 : lineH);
						}

						handle.style.display = 'flex';
						handle.style.top = `${anchorTop - wrapperRect.top + (anchorH - handleH) / 2}px`;
						handle.style.left = '-12px';
					}

					function hideHandle() {
						handle.style.display = 'none';
					}

					// --- Context menu ---
					function showMenu() {
						if (!activeBlock) return;
						menuOpen = true;
						editorView.dom.blur();

						// Highlight current block type
						const curType = currentBlockType(activeBlock);
						for (const mi of menuItems) {
							mi.el.classList.toggle('active', mi.type === curType);
						}

						// Position menu next to handle
						const handleRect = handle.getBoundingClientRect();
						const menuHeight = 380; // approximate
						const spaceBelow = window.innerHeight - handleRect.bottom;

						menu.style.display = 'block';
						menuBackdrop.style.display = 'block';

						if (spaceBelow > menuHeight || spaceBelow > handleRect.top) {
							// Show below
							menu.style.top = `${handleRect.bottom + 4}px`;
						} else {
							// Show above
							menu.style.top = `${handleRect.top - menuHeight - 4}px`;
						}
						menu.style.left = `${Math.max(8, handleRect.left - 8)}px`;
					}

					function hideMenu() {
						menuOpen = false;
						menu.style.display = 'none';
						menuBackdrop.style.display = 'none';
					}

					// Menu item clicks
					menu.addEventListener('click', (e) => {
						const btn = (e.target as HTMLElement).closest('.block-menu-item') as HTMLElement;
						if (!btn || !activeBlock) return;
						e.stopPropagation();

						const type = btn.dataset.type;
						if (type) {
							applyBlockType(tiptapEditor, activeBlock, type);
						}

						hideMenu();
						activeBlock = null;
						hideHandle();
					});

					duplicateBtn.addEventListener('click', (e) => {
						e.stopPropagation();
						if (!activeBlock) return;
						const { tr } = editorView.state;
						const insertPos = activeBlock.pos + activeBlock.node.nodeSize;
						tr.insert(insertPos, activeBlock.node.copy(activeBlock.node.content));
						editorView.dispatch(tr);
						hideMenu();
						activeBlock = null;
						hideHandle();
					});

					deleteBtn.addEventListener('click', (e) => {
						e.stopPropagation();
						if (!activeBlock) return;
						const { tr } = editorView.state;
						tr.delete(activeBlock.pos, activeBlock.pos + activeBlock.node.nodeSize);

						// Clean up empty list
						const checkPos = Math.min(activeBlock.pos, tr.doc.content.size);
						try {
							const $check = tr.doc.resolve(checkPos);
							for (let d = $check.depth; d > 0; d--) {
								const ancestor = $check.node(d);
								if (LIST_CONTAINERS.has(ancestor.type.name) && ancestor.childCount === 0) {
									const start = $check.before(d);
									tr.delete(start, start + ancestor.nodeSize);
									break;
								}
							}
						} catch {}

						editorView.dispatch(tr);
						hideMenu();
						activeBlock = null;
						hideHandle();
					});

					menuBackdrop.addEventListener('click', () => hideMenu());
					menuBackdrop.addEventListener('touchend', (e) => {
						e.preventDefault();
						hideMenu();
					});

					// --- Drop line ---
					function showDropLine(pos: number) {
						const wrapperRect = wrapper.getBoundingClientRect();
						const dom = editorView.nodeDOM(pos);
						if (dom instanceof HTMLElement) {
							const rect = dom.getBoundingClientRect();
							dropLine.style.display = 'block';
							dropLine.style.top = `${rect.top - wrapperRect.top - 1}px`;
							return;
						}
						const doc = editorView.state.doc;
						if (pos >= doc.content.size) {
							const last = wrapper.querySelector('.ProseMirror')?.lastElementChild as HTMLElement;
							if (last) {
								const rect = last.getBoundingClientRect();
								dropLine.style.display = 'block';
								dropLine.style.top = `${rect.bottom - wrapperRect.top}px`;
								return;
							}
						}
						dropLine.style.display = 'none';
					}

					function hideDropLine() {
						dropLine.style.display = 'none';
					}

					// --- Ghost clone ---
					function createGhost(blockDOM: HTMLElement) {
						ghost = blockDOM.cloneNode(true) as HTMLElement;
						const editorWidth = editorView.dom.offsetWidth;
						ghost.style.cssText = `
							position: fixed;
							width: ${Math.max(editorWidth, 200)}px;
							pointer-events: none;
							z-index: 1000;
							opacity: 0.85;
							background: var(--bg-secondary);
							box-shadow: 0 8px 24px rgba(0,0,0,0.25);
							border-radius: 6px;
							padding: 4px 12px;
							border: 2px solid var(--accent-blue);
							transform: scale(1.01);
							transition: none;
							max-height: 80px;
							overflow: hidden;
						`;
						document.body.appendChild(ghost);
					}

					function moveGhost(clientY: number) {
						if (!ghost) return;
						ghost.style.top = `${clientY - 20}px`;
						ghost.style.left = `${wrapper.getBoundingClientRect().left + 24}px`;
					}

					function removeGhost() {
						if (ghost) { ghost.remove(); ghost = null; }
					}

					// --- Auto-scroll ---
					let lastDragY = 0;

					function updateAutoScroll(clientY: number) {
						lastDragY = clientY;
						const sc = getScrollContainer();
						const rect = sc.getBoundingClientRect();
						const edge = 60;
						const maxSpeed = 12;
						let speed = 0;
						if (clientY < rect.top + edge) {
							speed = -maxSpeed * Math.max(0, 1 - (clientY - rect.top) / edge);
						} else if (clientY > rect.bottom - edge) {
							speed = maxSpeed * Math.max(0, 1 - (rect.bottom - clientY) / edge);
						}
						if (scrollRAF) cancelAnimationFrame(scrollRAF);
						if (Math.abs(speed) > 0.5) {
							const tick = () => {
								sc.scrollBy(0, speed);
								if (dragging && activeBlock) {
									currentDropPos = dropPosAtY(editorView, lastDragY, activeBlock.pos);
									if (currentDropPos !== null) showDropLine(currentDropPos);
								}
								scrollRAF = requestAnimationFrame(tick);
							};
							scrollRAF = requestAnimationFrame(tick);
						}
					}

					function stopAutoScroll() {
						if (scrollRAF) { cancelAnimationFrame(scrollRAF); scrollRAF = null; }
					}

					// --- Drag lifecycle ---
					function startDrag() {
						if (!activeBlock) return;
						hideMenu();
						dragging = true;
						activeBlock.dom.style.opacity = '0.2';
						handle.classList.add('active');
						editorView.dom.style.pointerEvents = 'none';
						editorView.dom.blur();
						createGhost(activeBlock.dom);
					}

					function moveDrag(clientY: number) {
						if (!dragging || !activeBlock) return;
						currentDropPos = dropPosAtY(editorView, clientY, activeBlock.pos);
						if (currentDropPos !== null) {
							showDropLine(currentDropPos);
						} else {
							hideDropLine();
						}
						moveGhost(clientY);
						updateAutoScroll(clientY);
					}

					function endDrag() {
						if (!dragging || !activeBlock) return;
						stopAutoScroll();
						editorView.dom.style.pointerEvents = '';

						if (currentDropPos !== null && currentDropPos !== activeBlock.pos) {
							try {
								executeMove(editorView, activeBlock.pos, activeBlock.node, currentDropPos);
							} catch (e) {
								console.warn('Block move failed:', e);
							}
						}

						if (activeBlock.dom) activeBlock.dom.style.opacity = '';
						handle.classList.remove('active');
						dragging = false;
						currentDropPos = null;
						activeBlock = null;
						hideDropLine();
						hideHandle();
						removeGhost();
					}

					function cancelDrag() {
						stopAutoScroll();
						editorView.dom.style.pointerEvents = '';
						if (activeBlock?.dom) activeBlock.dom.style.opacity = '';
						handle.classList.remove('active');
						dragging = false;
						pendingDrag = false;
						currentDropPos = null;
						hideDropLine();
						removeGhost();
					}

					// --- Mouse events ---
					function onMouseMove(e: MouseEvent) {
						if (dragging || menuOpen) return;
						hoverMode = true;
						userHasInteracted = true;
						const editorRect = editorView.dom.getBoundingClientRect();
						const block = blockAtCoords(editorView, editorRect.left + 50, e.clientY);
						if (block) {
							activeBlock = block;
							positionHandle(block.dom);
						} else {
							activeBlock = null;
							hideHandle();
						}
					}

					function onMouseUp() {
						if (dragging) endDrag();
					}

					function onMouseLeave() {
						if (!dragging && !menuOpen) {
							hoverMode = false;
							activeBlock = null;
							hideHandle();
						}
					}

					// Desktop: click = menu, mousedown+move = drag
					let mouseDownOnHandle = false;
					let mouseStartY = 0;

					handle.addEventListener('mousedown', (e) => {
						e.preventDefault();
						e.stopPropagation();
						mouseDownOnHandle = true;
						mouseStartY = e.clientY;
					});

					function onMouseMoveGlobal(e: MouseEvent) {
						if (mouseDownOnHandle && !dragging) {
							if (Math.abs(e.clientY - mouseStartY) > 5) {
								mouseDownOnHandle = false;
								startDrag();
							}
						}
						if (dragging) {
							moveDrag(e.clientY);
						}
					}

					function onMouseUpGlobal(e: MouseEvent) {
						if (mouseDownOnHandle && !dragging) {
							// It was a click — open menu
							mouseDownOnHandle = false;
							if (menuOpen) {
								hideMenu();
							} else {
								showMenu();
							}
							return;
						}
						mouseDownOnHandle = false;
						if (dragging) endDrag();
					}

					// --- Touch events ---
					handle.addEventListener('touchstart', (e) => {
						e.preventDefault();
						e.stopPropagation();
						pendingDrag = true;
						pendingTouchY = e.touches[0].clientY;
					}, { passive: false });

					function onTouchMove(e: TouchEvent) {
						const touchY = e.touches[0].clientY;
						if (pendingDrag && !dragging) {
							if (Math.abs(touchY - pendingTouchY) > 8) {
								startDrag();
								pendingDrag = false;
							} else {
								return;
							}
						}
						if (!dragging) return;
						e.preventDefault();
						moveDrag(touchY);
					}

					function onTouchEnd(e: TouchEvent) {
						if (pendingDrag) {
							// Tap on handle → open menu
							pendingDrag = false;
							if (menuOpen) {
								hideMenu();
							} else {
								showMenu();
							}
							return;
						}
						if (!dragging) return;
						e.preventDefault();
						endDrag();
					}

					function onEditorInteraction() {
						userHasInteracted = true;
					}

					// --- Attach listeners ---
					wrapper.addEventListener('mousemove', onMouseMove);
					wrapper.addEventListener('mouseleave', onMouseLeave);
					wrapper.addEventListener('click', onEditorInteraction);
					wrapper.addEventListener('touchend', onEditorInteraction, { passive: true });
					window.addEventListener('mousemove', onMouseMoveGlobal);
					window.addEventListener('mouseup', onMouseUpGlobal);
					window.addEventListener('touchmove', onTouchMove, { passive: false });
					window.addEventListener('touchend', onTouchEnd, { passive: false });

					return {
						update(view) {
							if (dragging || menuOpen) return;
							if (hoverMode) return;
							if (!userHasInteracted) return;

							const { selection } = view.state;
							if (selection.empty) {
								const block = blockAtPos(view, selection.from);
								if (block) {
									activeBlock = block;
									positionHandle(block.dom);
								} else {
									activeBlock = null;
									hideHandle();
								}
							}
						},
						destroy() {
							stopAutoScroll();
							removeGhost();
							hideMenu();
							handle.remove();
							dropLine.remove();
							menu.remove();
							menuBackdrop.remove();
							wrapper.removeEventListener('mousemove', onMouseMove);
							wrapper.removeEventListener('mouseleave', onMouseLeave);
							wrapper.removeEventListener('click', onEditorInteraction);
							window.removeEventListener('mousemove', onMouseMoveGlobal);
							window.removeEventListener('mouseup', onMouseUpGlobal);
							window.removeEventListener('touchmove', onTouchMove);
							window.removeEventListener('touchend', onTouchEnd);
						},
					};
				},
			}),
		];
	},
});
