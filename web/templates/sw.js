// Seyal's service worker. It makes the app installable, opens it quickly by keeping the static files,
// and shows an "offline" page instead of a browser error. It never keeps pages with business data:
// those always come fresh from the server. It also shows phone notes (web push) and opens Seyal
// where a tapped one points, even when no Seyal page is open.
//
// What it keeps on this phone (D93): when someone shares a voice note, photo, PDF or words into Seyal
// from another app's Share menu (Android), it keeps that one share in the browser's storage (IndexedDB,
// sharebox.js) until the person sends it from Tell Seyal or discards it, with the login last seen on
// this phone. It is never sent anywhere by the service worker, and is cleared after 24 hours (the next
// time Seyal runs here), on log out, and when another login opens Seyal on this phone.
"use strict";

const VERSION = "__VERSION__"; // the static files' fingerprint, filled in by the server
importScripts("/static/sharebox.js?v=" + VERSION);
const CACHE = "seyal-" + VERSION;
const OFFLINE = "/offline?v=" + VERSION;
const PRECACHE = [
  OFFLINE,
  "/static/app.css?v=" + VERSION,
  "/static/fonts.css?v=" + VERSION,
  "/static/app.js?v=" + VERSION,
  "/static/links.js?v=" + VERSION,
  "/static/sharebox.js?v=" + VERSION,
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
  // Something shared into Seyal from another app (the manifest's share_target): kept on this phone.
  if (req.method === "POST" && url.origin === self.location.origin && SHARE.test(url.pathname)) {
    event.respondWith(catchShare(req, url.pathname.slice(0, -"share".length)));
    return;
  }
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

// --- Share in (D93) ------------------------------------------------------------------------------
// The Share menu posts the shared things here, from no page and with no login (it can't carry one). So
// the service worker keeps them on the phone and opens Tell Seyal; the person sends them from there,
// logged in, with a tap. A form on any website could post the same: that is why it only fills the
// banner ("Send it only if you just shared it"), keeps only the newest share, with caps, and nothing
// is ever sent or filled in without the person's tap. Only the newest share is kept.
const SHARE = /^\/t\/[a-z0-9][a-z0-9-]{1,62}\/share$/;

async function catchShare(req, base) {
  const tell = (why) => Response.redirect(new URL(base + "?view=tell&share=" + why, self.location.origin).href, 303);
  try {
    const form = await req.formData();
    const files = [];
    for (const f of form.getAll("files")) {
      if (f && typeof f === "object" && typeof f.size === "number") files.push({ name: f.name, type: f.type, size: f.size, data: f });
    }
    const owner = await SeyalShareBox.lastLogin();
    const rec = SeyalShareBox.make({ title: form.get("title"), text: form.get("text"), url: form.get("url") },
      files, Date.now(), owner, self.crypto.randomUUID());
    await SeyalShareBox.put(rec);
    return tell("waiting");
  } catch (_) {
    return tell("missed"); // Tell Seyal says to open Seyal once and share again
  }
}

// expire clears a share left waiting for more than a day. A phone has no timer Seyal can use, so it
// runs whenever Seyal runs here: when the service worker starts, on a phone note, and on every page.
function expire() {
  return SeyalShareBox.get().then((rec) => {
    if (rec && !(Math.abs(Date.now() - rec.at) < SeyalShareBox.LIFE)) return SeyalShareBox.clear();
  }).catch(() => { /* nothing kept */ });
}
expire();

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
  event.waitUntil(Promise.all([expire(), self.registration.showNotification(title, {
    body, tag, icon: "/static/icon-192.png", data: { link: link(note.navigate) },
  })]));
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
