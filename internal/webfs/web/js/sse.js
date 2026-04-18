// EventSource wrapper with tidy callbacks.
export function subscribe(url, onEvent, onError) {
  const src = new EventSource(url, { withCredentials: true });
  src.addEventListener('event', (e) => {
    try { onEvent(JSON.parse(e.data), e); }
    catch { onEvent(e.data, e); }
  });
  src.onmessage = (e) => {
    try { onEvent(JSON.parse(e.data), e); }
    catch { /* keep-alive or non-JSON */ }
  };
  src.onerror = (e) => { if (onError) onError(e); };
  return () => src.close();
}
