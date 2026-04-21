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

    const col = st.lastDropZone;
    if (!col) return;
    const toStatus = col.getAttribute('data-status');

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
