// Fetch wrapper: same-origin, JSON in/out, 401 → redirect to /login.
export async function api(path, opts = {}) {
  const options = Object.assign({ credentials: 'same-origin', headers: {} }, opts);
  if (options.body && typeof options.body === 'object' && !(options.body instanceof FormData)) {
    options.headers['Content-Type'] = 'application/json';
    options.body = JSON.stringify(options.body);
  }
  const res = await fetch(path, options);
  if (res.status === 401) {
    if (location.pathname !== '/login') {
      location.href = '/login';
    }
    throw new Error('unauthorized');
  }
  const text = await res.text();
  let data;
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (!res.ok) {
    const err = new Error(data && data.error ? data.error : ('HTTP ' + res.status));
    err.status = res.status;
    err.body = data;
    throw err;
  }
  return data;
}
