// Wraps EventSource. Backend emits unnamed frames (see writeSSE in
// internal/server/handlers.go), so every message arrives through onmessage;
// the original event name is included as `event` inside the JSON payload.
export function subscribe(url, onEvent, onError) {
  const src = new EventSource(url, { withCredentials: true });
  src.onmessage = (e) => {
    if (!e.data || e.data[0] !== '{') return; // ignore keep-alive comments
    try { onEvent(JSON.parse(e.data), e); } catch { /* non-JSON */ }
  };
  src.onerror = (e) => { if (onError) onError(e); };
  return () => src.close();
}
