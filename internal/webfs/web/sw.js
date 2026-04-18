// Minimal service worker: cache app shell; API / SSE are never cached.
const CACHE = 'hermes-taskboard-v1';
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
  '/favicon.svg',
  '/manifest.webmanifest',
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

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (url.pathname.startsWith('/api/')) return; // network-first, never cached
  if (e.request.method !== 'GET') return;
  e.respondWith(
    caches.match(e.request).then((cached) => cached ||
      fetch(e.request).then((res) => {
        if (res.ok && (url.origin === self.location.origin)) {
          const clone = res.clone();
          caches.open(CACHE).then((c) => c.put(e.request, clone));
        }
        return res;
      }).catch(() => caches.match('/index.html')))
  );
});
