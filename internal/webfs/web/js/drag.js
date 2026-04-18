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

    // Hide the source (reserve its space via a placeholder so other cards don't jump).
    const placeholder = document.createElement('div');
    placeholder.className = 'card-drop-placeholder';
    placeholder.style.height = rect.height + 'px';
    sourceEl.parentNode.insertBefore(placeholder, sourceEl);
    sourceEl.style.display = 'none';

    // Capture pointer so we keep events even off the source.
    try { sourceEl.setPointerCapture(event.pointerId); } catch {}

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

    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', end);
    window.addEventListener('pointercancel', cancel);
    document.body.classList.add('dragging-active');
    event.preventDefault();
  }

  function move(event) {
    if (!state) return;
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
    if (st.sourceEl) st.sourceEl.style.display = '';
    placeholder.remove();

    onDrop({ taskId: st.taskId, toStatus, beforeId, afterId });
  }

  function cancel() {
    if (!state) return;
    if (state.sourceEl) state.sourceEl.style.display = '';
    if (state.placeholder && state.placeholder.parentNode) state.placeholder.remove();
    cleanup();
  }

  function cleanup() {
    if (!state) return;
    state.clone.remove();
    window.removeEventListener('pointermove', move);
    window.removeEventListener('pointerup', end);
    window.removeEventListener('pointercancel', cancel);
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
