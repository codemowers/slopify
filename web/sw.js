// Caches the app shell so the PWA opens instantly and offline. Library data
// and streams always go to the network; offline pinning comes later.
const CACHE = "slopify-shell-v4";
const SHELL = ["/", "/app.js", "/media.js", "/network.js", "/style.css", "/manifest.webmanifest", "/icon.svg"];

self.addEventListener("install", (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== "GET" || url.origin !== location.origin || !SHELL.includes(url.pathname)) return;
  // Network first keeps the shell current; the cache answers when offline.
  e.respondWith(
    fetch(e.request)
      .then(async (res) => {
        if ([502, 503, 504].includes(res.status)) {
          const cached = await caches.match(e.request);
          if (cached) return cached;
        }
        if (res.ok && !res.redirected) {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(e.request, copy));
        }
        return res;
      })
      .catch(() => caches.match(e.request)),
  );
});
