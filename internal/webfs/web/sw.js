// Minimal service worker: cache app shell; API / SSE are never cached.
const CACHE = 'hermes-taskboard-v14';
const SHELL = [
  '/',
  '/index.html',
  '/assets/app.css',
  '/assets/animations.css',
  '/assets/responsive.css',
  '/assets/vue.global.js',
  '/js/app.js',
  '/js/i18n.js',
  '/js/sound.js',
  '/js/pwa.js',
  '/js/api.js',
  '/js/sse.js',
  '/js/version.js',
  '/favicon.svg',
  '/manifest.webmanifest',
  '/assets/icons/icon-192.png',
  '/assets/icons/icon-512.png',
  '/assets/icons/apple-touch-icon.png',
  '/locales/en.json',
  '/locales/zh-CN.json',
];

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL).catch(() => null)));
  self.skipWaiting();
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys().then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
  );
  self.clients.claim();
});

// Network-first for app-shell assets. The alternative (cache-first) made
// users stare at stale CSS / JS after a deploy until they cleared site
// data, which was a constant source of "bug still there" reports. Cache
// is now an offline fallback only.
self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (url.pathname.startsWith('/api/')) return; // backend, never cached
  if (e.request.method !== 'GET') return;
  e.respondWith(
    fetch(e.request).then((res) => {
      if (res.ok && url.origin === self.location.origin) {
        const clone = res.clone();
        caches.open(CACHE).then((c) => c.put(e.request, clone));
      }
      return res;
    }).catch(() => caches.match(e.request).then((c) => c || caches.match('/index.html')))
  );
});
