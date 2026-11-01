// Seyal's service worker. It makes the app installable, opens it quickly by keeping the static files,
// and shows an "offline" page instead of a browser error. It never keeps pages with business data:
// those always come fresh from the server.
"use strict";

const VERSION = "__VERSION__"; // the static files' fingerprint, filled in by the server
const CACHE = "seyal-" + VERSION;
const OFFLINE = "/offline?v=" + VERSION;
const PRECACHE = [
  OFFLINE,
  "/static/app.css?v=" + VERSION,
  "/static/fonts.css?v=" + VERSION,
  "/static/app.js?v=" + VERSION,
  "/static/htmx.min.js?v=" + VERSION,
  "/static/icon-192.png",
];

self.addEventListener("install", (event) => {
  event.waitUntil(caches.open(CACHE).then((c) => c.addAll(PRECACHE)).then(() => self.skipWaiting()));
});

// A new version replaces the old one's files.
self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k.startsWith("seyal-") && k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener("fetch", (event) => {
  const req = event.request;
  const url = new URL(req.url);
  if (req.method !== "GET" || url.origin !== self.location.origin) return;

  // Pages: always from the network; if that fails, the offline page.
  if (req.mode === "navigate") {
    event.respondWith(fetch(req).catch(() => caches.match(OFFLINE)));
    return;
  }
  // Static files (fingerprinted, so safe to keep): from the cache first.
  if (url.pathname.startsWith("/static/")) {
    event.respondWith(
      caches.match(req).then((hit) => hit || fetch(req).then((res) => {
        if (res.ok) {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(req, copy));
        }
        return res;
      }))
    );
  }
  // Everything else (voice notes, uploads, forms) goes to the network as usual.
});
