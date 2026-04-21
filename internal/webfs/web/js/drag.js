// Pointer-based card drag with a floating clone and live-preview placeholder.
// Replaces HTML5 Drag-and-Drop so we get the Trello-style behaviour users
// actually want: the source card disappears smoothly, a styled clone follows
// the cursor, a dotted placeholder shows where the card will land, and the
// drop commits against either a sibling card (for ordering) or a column.
//
// Usage:
//   const drag = createDragController({
//     onDrop({ taskId, toStatus, beforeId, afterId }) { ... },
//   });
//   <div class="card" @pointerdown="e => drag.start(e, task.id, $event.currentTarget)">
//   <div class="column" data-status="draft">  ← must have data-status

export function createDragController(opts) {
  const onDrop = opts.onDrop || (() => {});
  let state = null;

  function start(event, taskId, sourceEl) {
    if (event.button !== 0) return; // left button only
    // Don't start a drag if the pointerdown originated on a control.
    if (event.target.closest('button, input, textarea, select, a')) return;

    const rect = sourceEl.getBoundingClientRect();
    const offsetX = event.clientX - rect.left;
    const offsetY = event.clientY - rect.top;

    // Build the floating clone.
    const clone = sourceEl.cloneNode(true);
    clone.classList.add('card-drag-clone');
    clone.style.width = rect.width + 'px';
    clone.style.left = rect.left + 'px';
    clone.style.top = rect.top + 'px';
    document.body.appendChild(clone);

    // Reserve space via a placeholder. The source element is then moved
    // off-screen (NOT display:none) — display:none drops the implicit
    // pointer capture that touch placed on the source's touch target,
    // which Chromium reports as a pointercancel and wipes the drag a
    // single move event in. position:absolute + far-offscreen keeps the
    // element in the render tree and the touch alive, while the
    // placeholder fills the original slot visually.
    const placeholder = document.createElement('div');
    placeholder.className = 'card-drop-placeholder';
    placeholder.style.height = rect.height + 'px';
    sourceEl.parentNode.insertBefore(placeholder, sourceEl);
    // Tuck the source out of view so the placeholder visually replaces it.
    // position+offscreen rather than display:none so the touch's implicit
    // capture target stays in the render tree.
    sourceEl.dataset.dragSavedStyle = sourceEl.getAttribute('style') || '';
    sourceEl.style.position = 'absolute';
    sourceEl.style.left = '-99999px';
    sourceEl.style.top = '0px';
    sourceEl.style.pointerEvents = 'none';
    sourceEl.style.visibility = 'hidden';

    state = {
      taskId,
      sourceEl,
      clone,
      placeholder,
      offsetX,
      offsetY,
      pointerId: event.pointerId,
      lastDropZone: null, // column the placeholder currently sits in
    };

    window.addEventListener('pointermove', move, { passive: false });
    window.addEventListener('pointerup', end);
    window.addEventListener('pointercancel', cancel);
    // Hijack native touchmove with passive:false. preventDefault on a
    // raw touchmove is the ONLY reliable way to override the touch-
    // action: auto pan that the browser committed to at touchstart —
    // calling preventDefault on the higher-level pointermove fires too
    // late and Chromium responds with a pointercancel that wipes the
    // drag a single move in.
    window.addEventListener('touchmove', preventTouchScroll, { passive: false });
    document.body.classList.add('dragging-active');
    try { event.preventDefault(); } catch {}
  }

  function preventTouchScroll(ev) {
    try { ev.preventDefault(); } catch {}
  }

  function move(event) {
    if (!state) return;
    try { event.preventDefault(); } catch {}
    state.clone.style.left = (event.clientX - state.offsetX) + 'px';
    state.clone.style.top = (event.clientY - state.offsetY) + 'px';

    // Cross-column drop on mobile: every other column is display:none
    // (single-tab view), so elementFromPoint only ever hits the current
    // column. To let users drop into a different column we treat the
    // .board-tabs buttons themselves as drop targets — finger over a
    // tab = card goes to that tab's column when released.
    const hit = document.elementFromPoint(event.clientX, event.clientY);
    const tabBtn = hit && hit.closest && hit.closest('.board-tabs button');
    if (tabBtn) {
      // Clear previous tab highlights and mark this one.
      const allTabs = document.querySelectorAll('.board-tabs button');
      for (const b of allTabs) b.classList.remove('drop-target');
      tabBtn.classList.add('drop-target');
      // Map tab → column by index (the v-for renders them in lockstep).
      const tabs = [...allTabs];
      const cols = [...document.querySelectorAll('.column[data-status]')];
      const col = cols[tabs.indexOf(tabBtn)];
      if (col) {
        state.lastDropZone = col;
        // Tuck the placeholder inside the hidden column. It won't show
        // visually, but end() reads col.data-status from lastDropZone.
        const zone = col.querySelector('.column-drop-zone') || col;
        if (state.placeholder.parentNode !== zone) zone.appendChild(state.placeholder);
      }
      return;
    }
    // Not over a tab — clear any tab highlight from a previous frame.
    const lit = document.querySelector('.board-tabs button.drop-target');
    if (lit) lit.classList.remove('drop-target');

    // Find the column under the cursor.
    const col = columnAt(event.clientX, event.clientY);
    if (!col) return;
    const zone = col.querySelector('.column-drop-zone') || col;

    // Find the card (other than our source) whose vertical midpoint is below
    // the cursor — that's our "insert before" target. If none, insert at end.
    const cards = [...zone.querySelectorAll('.card:not(.card-drag-clone)')]
      .filter((c) => c !== state.sourceEl);
    let insertBefore = null;
    for (const c of cards) {
      const r = c.getBoundingClientRect();
      if (event.clientY < r.top + r.height / 2) { insertBefore = c; break; }
    }

    // Move the placeholder.
    if (insertBefore) zone.insertBefore(state.placeholder, insertBefore);
    else zone.appendChild(state.placeholder);

    state.lastDropZone = col;
  }

  function end() {
    if (!state) return;
    const st = state;
    cleanup();
    // Wipe any tab-drop highlight left from the last move().
    const lit = document.querySelector('.board-tabs button.drop-target');
    if (lit) lit.classList.remove('drop-target');

    const col = st.lastDropZone;
    if (!col) return;
    const toStatus = col.getAttribute('data-status');
    // Cross-tab drop on mobile: switch the visible tab to the destination
    // column so the user immediately sees the card after the drop.
    const visibleCol = document.querySelector('.column:not(.hidden-mobile)[data-status]');
    if (visibleCol && visibleCol !== col) {
      const tabs = [...document.querySelectorAll('.board-tabs button')];
      const cols = [...document.querySelectorAll('.column[data-status]')];
      const idx = cols.indexOf(col);
      if (idx >= 0 && tabs[idx]) tabs[idx].click();
    }

    // Compute beforeId / afterId from placeholder neighbors.
    const placeholder = st.placeholder;
    let beforeId = '', afterId = '';
    const next = nextSiblingCard(placeholder);
    const prev = prevSiblingCard(placeholder);
    if (next) beforeId = next.getAttribute('data-task-id') || '';
    if (prev) afterId = prev.getAttribute('data-task-id') || '';

    // Restore source cell; we'll let the state change rerender the list.
    restoreSource(st.sourceEl);
    placeholder.remove();

    onDrop({ taskId: st.taskId, toStatus, beforeId, afterId });
  }

  function cancel() {
    if (!state) return;
    restoreSource(state.sourceEl);
    if (state.placeholder && state.placeholder.parentNode) state.placeholder.remove();
    cleanup();
  }

  function restoreSource(el) {
    if (!el) return;
    const saved = el.dataset.dragSavedStyle;
    if (saved !== undefined) {
      if (saved) el.setAttribute('style', saved); else el.removeAttribute('style');
      delete el.dataset.dragSavedStyle;
    } else {
      // Fallback if we never stashed a snapshot.
      el.style.position = '';
      el.style.left = '';
      el.style.top = '';
      el.style.pointerEvents = '';
      el.style.visibility = '';
      el.style.display = '';
    }
  }

  function cleanup() {
    if (!state) return;
    state.clone.remove();
    window.removeEventListener('pointermove', move, { passive: false });
    window.removeEventListener('pointerup', end);
    window.removeEventListener('pointercancel', cancel);
    window.removeEventListener('touchmove', preventTouchScroll, { passive: false });
    document.body.classList.remove('dragging-active');
    state = null;
  }

  return { start };
}

function columnAt(x, y) {
  const el = document.elementFromPoint(x, y);
  if (!el) return null;
  return el.closest('.column[data-status]');
}

function nextSiblingCard(el) {
  let n = el.nextElementSibling;
  while (n && !n.classList.contains('card')) n = n.nextElementSibling;
  return n;
}
function prevSiblingCard(el) {
  let n = el.previousElementSibling;
  while (n && !n.classList.contains('card')) n = n.previousElementSibling;
  return n;
}
