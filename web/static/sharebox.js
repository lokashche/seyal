// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Seyal's waiting share (D93): something shared into Seyal from another app's Share menu (Android),
// kept on this phone until the person presses Send on Tell Seyal, or Discard. The service worker
// (sw.js) makes it from the Share menu's post; the pages (app.js) show it and send it. Nothing in here
// talks to the server.
//
// The first part is pure functions with no page in them, so a test runs them in Node
// (web/sharebox_test.go). The second part keeps the share in the browser's own storage (IndexedDB):
// one share at a time, phone-wide (not per business, so [change] can move it), with the login last
// seen on this phone.
var SeyalShareBox = (function () {
  "use strict";

  var MAX_FILES = 5;                   // as many files as one message may carry (server maxFiles)
  var MAX_BYTES = 25 * 1024 * 1024;    // all files together (one file's limit on the server, media.MaxSize)
  var MAX_TEXT = 10000;                // the longest message (channel.MaxText); longer words are cut
  var LIFE = 24 * 3600 * 1000;         // a share left waiting longer is cleared the next time Seyal runs

  // The kinds of file Seyal keeps (internal/media), and what a file's name says when the phone gives no
  // usable type. A WhatsApp voice note is .opus, often sent as application/octet-stream.
  var KEEP = ["audio/webm", "audio/ogg", "audio/mp4", "audio/mpeg", "audio/aac", "audio/wav", "audio/x-m4a",
    "image/png", "image/jpeg", "image/gif", "image/webp", "image/heic", "application/pdf"];
  var BY_NAME = { opus: "audio/ogg", ogg: "audio/ogg", oga: "audio/ogg", m4a: "audio/x-m4a", mp3: "audio/mpeg",
    aac: "audio/aac", wav: "audio/wav", webm: "audio/webm", pdf: "application/pdf", jpg: "image/jpeg",
    jpeg: "image/jpeg", png: "image/png", gif: "image/gif", webp: "image/webp", heic: "image/heic" };

  // fileType is the type Seyal keeps a file as, or "" when Seyal can't keep it (a Word file, a contact).
  function fileType(name, type) {
    var base = String(type || "").split(";")[0].trim().toLowerCase();
    if (KEEP.indexOf(base) >= 0) return base;
    if (base === "" || base === "application/octet-stream" || base === "audio/opus") {
      var ext = String(name || "").toLowerCase().split(".").pop();
      if (BY_NAME[ext]) return BY_NAME[ext];
    }
    return "";
  }

  // joinText joins the shared title, text and link (Android puts a link in text, not in url), leaving out
  // a part that an earlier one already holds.
  function joinText(title, text, url) {
    var out = [];
    [title, text, url].forEach(function (p) {
      p = String(p || "").trim();
      if (p && !out.some(function (o) { return o.indexOf(p) >= 0; })) out.push(p);
    });
    return out.join("\n");
  }

  // make builds the waiting share from what the Share menu posted: fields {title, text, url}, and files
  // [{name, type, size, data}]. Only files Seyal keeps, at most MAX_FILES and MAX_BYTES; the others are
  // named in left ({name, why: "type" | "count" | "size"}). Words over MAX_TEXT letters are cut (cut).
  // owner is the login last seen on this phone ("" when none yet: the first login to open it owns it).
  function make(fields, files, now, owner, id) {
    var text = joinText(fields.title, fields.text, fields.url);
    var letters = Array.from(text);
    var rec = { id: id, at: now, owner: owner || "", text: text, cut: false, files: [], left: [] };
    if (letters.length > MAX_TEXT) {
      rec.text = letters.slice(0, MAX_TEXT).join("");
      rec.cut = true;
    }
    var bytes = 0;
    (files || []).forEach(function (f) {
      var type = fileType(f.name, f.type);
      var why = !type ? "type" : rec.files.length === MAX_FILES ? "count" : bytes + f.size > MAX_BYTES ? "size" : "";
      if (why) {
        rec.left.push({ name: f.name || "a file", why: why });
        return;
      }
      bytes += f.size;
      rec.files.push({ name: f.name || "shared file", type: type, size: f.size, data: f.data });
    });
    return rec;
  }

  // decide says what a page does with the waiting share (rec, or null) for the login on it (login, ""
  // on a page with nobody logged in): "none" (nothing waits), "clear" (older than LIFE, or another
  // login's), "keep" (wait for a login), "claim" (nobody owned it yet: this login now does) or "show".
  function decide(rec, login, now) {
    if (!rec) return "none";
    if (!(Math.abs(now - rec.at) < LIFE)) return "clear"; // also when the clock went back by a day or more
    if (!login) return "keep";
    if (!rec.owner) return "claim";
    return rec.owner === login ? "show" : "clear";
  }

  // afterSend says what happens to the share after a Send got the answer status (0: no answer) from the
  // page at path (where the answer was sent on to): only the server's OK clears it. A refusal, no answer,
  // or a login page (the session ran out, or a new password is needed first, or the login isn't in this
  // business) keeps it on the phone, to try again.
  function afterSend(status, path) {
    var ok = status >= 200 && status < 300 && !/\/(login|password)$/.test(path || "") && path !== "/";
    return ok ? "clear" : "keep";
  }

  // mayTry says whether the waiting share rec may be sent into the business place (its address name).
  // A Send whose answer never came (no connection, too slow) may have reached its business all the
  // same; that business is kept in rec.tried, and until it answers, the share goes only there. The
  // server's check for a share sent twice works inside one business only (D93).
  function mayTry(rec, place) {
    return !rec || !rec.tried || rec.tried === place;
  }

  // --- Keeping it on this phone (IndexedDB) ---------------------------------------------------------
  var DB = "seyal-share", STORE = "box", WAITING = "waiting", LOGIN = "login";

  function open() {
    return new Promise(function (resolve, reject) {
      if (typeof indexedDB === "undefined") { reject(new Error("no storage")); return; }
      var req = indexedDB.open(DB, 1);
      req.onupgradeneeded = function () { req.result.createObjectStore(STORE); };
      req.onsuccess = function () { resolve(req.result); };
      req.onerror = function () { reject(req.error); };
    });
  }
  function run(mode, fn) {
    return open().then(function (db) {
      return new Promise(function (resolve, reject) {
        var tx = db.transaction(STORE, mode), out;
        var req = fn(tx.objectStore(STORE));
        if (req) req.onsuccess = function () { out = req.result; };
        tx.oncomplete = function () { db.close(); resolve(out); };
        tx.onerror = tx.onabort = function () { db.close(); reject(tx.error); };
      });
    });
  }
  // get is the waiting share, or null (also when the browser keeps no storage).
  function get() {
    return run("readonly", function (s) { return s.get(WAITING); }).then(function (r) { return r || null; }, function () { return null; });
  }
  // put keeps rec as the waiting share; a newer share replaces an older one.
  function put(rec) { return run("readwrite", function (s) { return s.put(rec, WAITING); }); }
  function clear() { return run("readwrite", function (s) { return s.delete(WAITING); }).catch(function () { /* nothing kept */ }); }
  // noteLogin keeps the login seen on this page (a fingerprint, body data-sharer), for the next share.
  function noteLogin(login) {
    return run("readwrite", function (s) { return s.put(login, LOGIN); }).catch(function () { /* fine without it */ });
  }
  // forget forgets the login last seen here (on log out): a share made after that is the next login's.
  function forget() {
    return run("readwrite", function (s) { return s.delete(LOGIN); }).catch(function () { /* nothing kept */ });
  }
  function lastLogin() {
    return run("readonly", function (s) { return s.get(LOGIN); }).then(function (l) { return l || ""; }, function () { return ""; });
  }
  // tried keeps (or, with "", drops) the business a Send went to while its answer is awaited.
  function tried(rec, place) {
    if (place) rec.tried = place;
    else delete rec.tried;
    return put(rec).catch(function () { /* only a guard */ });
  }
  // claim makes login the owner of the waiting share rec.
  function claim(rec, login) {
    rec.owner = login;
    return put(rec);
  }

  return { MAX_FILES: MAX_FILES, MAX_BYTES: MAX_BYTES, MAX_TEXT: MAX_TEXT, LIFE: LIFE,
    fileType: fileType, joinText: joinText, make: make, decide: decide, afterSend: afterSend, mayTry: mayTry,
    get: get, put: put, clear: clear, claim: claim, tried: tried, noteLogin: noteLogin, forget: forget, lastLogin: lastLogin };
})();
if (typeof module !== "undefined" && module.exports) module.exports = SeyalShareBox;
