// Seyal's service worker. It makes the app installable, opens it quickly by keeping the static files,
// and shows an "offline" page instead of a browser error. It never keeps pages with business data:
// those always come fresh from the server. It also shows phone notes (web push) and opens Seyal
// where a tapped one points, even when no Seyal page is open.
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

// --- Phone notes -------------------------------------------------------------------------------
// A phone note arrives locked for this browser; the browser unlocks it before this runs. Its shape:
// {"web_push":8030,"notification":{"title":"Seyal","body":"…","navigate":"https://…/t/…/items/…","tag":"…"}}.
// A push ALWAYS shows a note: Safari takes the permission back after a push that shows nothing, and
// Chrome shows a note of its own. The note never holds more than the item number and who sent it.

// link keeps a note's link only if it is a Seyal page on this same server; anything else opens "/".
function link(address) {
  try {
    const url = new URL(address, self.location.origin);
    if (url.origin === self.location.origin && url.pathname.startsWith("/t/")) return url.href;
  } catch (_) { /* not an address */ }
  return self.location.origin + "/";
}

self.addEventListener("push", (event) => {
  let note = {};
  try {
    note = (event.data && event.data.json().notification) || {};
  } catch (_) { /* not the shape above: show the plain note */ }
  const title = typeof note.title === "string" && note.title ? note.title.slice(0, 60) : "Seyal";
  const body = typeof note.body === "string" && note.body ? note.body.slice(0, 200) : "Something new is waiting for you in Seyal.";
  const tag = typeof note.tag === "string" && note.tag ? note.tag.slice(0, 64) : "seyal";
  event.waitUntil(self.registration.showNotification(title, {
    body, tag, icon: "/static/icon-192.png", data: { link: link(note.navigate) },
  }));
});

// Tapping a note closes it. A Seyal window already on its link is brought to the front; otherwise
// the link opens in a new window. An open window on another page is never moved to the link: it may
// hold a voice note being recorded or a message not sent yet, and moving it would lose that. Only if
// no new window can be opened is an open Seyal window brought to the front, as it is.
self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const target = link(event.notification.data && event.notification.data.link);
  event.waitUntil((async () => {
    const open = (await self.clients.matchAll({ type: "window", includeUncontrolled: true }))
      .filter((win) => new URL(win.url).origin === self.location.origin);
    for (const win of open) {
      if (win.url.split("#")[0] !== target) continue;
      try {
        await win.focus();
        return;
      } catch (_) { /* that window can't be brought to the front: open a new one */ }
    }
    try {
      await self.clients.openWindow(target); // it may answer null when the window opened elsewhere
      return;
    } catch (_) { /* no new window allowed: bring an open one to the front, below */ }
    for (const win of open) {
      try {
        await win.focus();
        return;
      } catch (_) { /* try the next one */ }
    }
  })());
});
