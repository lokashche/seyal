// Seyal's own script: keep the newest message in view, Ctrl/Cmd+Enter sends, record voice notes,
// and choose or search what to start on Tell Seyal.
(function () {
  "use strict";

  function settle() {
    var thread = document.getElementById("thread");
    if (thread) thread.scrollTop = thread.scrollHeight;
  }
  document.addEventListener("DOMContentLoaded", settle);
  document.addEventListener("htmx:afterSettle", settle);

  // --- Chat | Details on phones ---------------------------------------------------------------
  // A work item shows its chat or its details. The choice is kept for that item only, so saving a
  // detail comes back to the details, and opening another item starts with its chat.

  function showPane(details) {
    var app = document.querySelector(".app.has-item");
    if (!app) return;
    app.classList.toggle("show-details", details);
    app.querySelectorAll("[data-show]").forEach(function (b) {
      b.setAttribute("aria-pressed", String((b.getAttribute("data-show") === "details") === details));
    });
    if (!details) settle();
  }
  function restorePane() {
    var kept = null;
    try { kept = sessionStorage.getItem("seyal-details"); } catch (_) { /* storage blocked: start with the chat */ }
    showPane(kept === location.pathname);
  }
  document.addEventListener("DOMContentLoaded", restorePane);
  document.addEventListener("htmx:afterSettle", restorePane);
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-show]");
    if (!b) return;
    var details = b.getAttribute("data-show") === "details";
    showPane(details);
    try {
      if (details) sessionStorage.setItem("seyal-details", location.pathname);
      else sessionStorage.removeItem("seyal-details");
    } catch (_) { /* fine without it */ }
  });

  document.addEventListener("keydown", function (e) {
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey) && e.target.matches(".composer textarea")) {
      e.preventDefault();
      e.target.form.requestSubmit();
    }
  });

  // --- Voice notes ------------------------------------------------------------------------
  // Record with the browser's MediaRecorder, let the person listen back, then send the recording
  // with the composer form (plus any text typed). The server stores it like any other file.
  //
  // Speak instead of type: when the business has a speech service (the form has data-transcribe),
  // the recording is sent at once to be turned into words, and nothing is kept on the server. The
  // words appear in the message box to be corrected; when the person presses Send, the recording goes
  // with the message, with the signed token that says what was heard (voice_token). If no words can be
  // made, the recording can still be sent as a voice note. Discard keeps nothing.
  //
  // A recording belongs to its page: going to another page (htmx swaps the page in place, so this
  // script keeps running) or closing it switches the microphone off and drops the recording, without
  // sending it anywhere (abandon).

  var MAX_SECONDS = 300; // stop by itself after 5 minutes
  var rec = null;        // the recording in progress: {recorder, stream, chunks, timer, started, form}

  function pickType() {
    var types = ["audio/webm;codecs=opus", "audio/ogg;codecs=opus", "audio/mp4"];
    for (var i = 0; i < types.length; i++) {
      if (window.MediaRecorder && MediaRecorder.isTypeSupported(types[i])) return types[i];
    }
    return "";
  }

  function clock(seconds) {
    var m = Math.floor(seconds / 60), s = seconds % 60;
    return m + ":" + (s < 10 ? "0" : "") + s;
  }

  function status(form, text) {
    var el = form.querySelector("[data-voice-status]");
    if (el) el.textContent = text;
  }

  var asking = false; // waiting for the microphone permission: a second click mustn't start another

  function tokenInput(form) { return form.querySelector('input[name="voice_token"]'); }

  function extension(blob) {
    return blob.type.indexOf("mp4") >= 0 ? "m4a" : blob.type.indexOf("ogg") >= 0 ? "ogg" : "webm";
  }

  function start(button) {
    var form = button.form;
    if (tokenInput(form) || form.dataset.transcribing) {
      status(form, "Send or discard this voice note first.");
      return;
    }
    if (!navigator.mediaDevices || !window.MediaRecorder) {
      status(form, "This browser can't record voice notes.");
      return;
    }
    if (asking) return;
    asking = true;
    navigator.mediaDevices.getUserMedia({ audio: true }).then(function (stream) {
      asking = false;
      if (!form.isConnected) { // the page changed while the browser asked: don't record
        stream.getTracks().forEach(function (t) { t.stop(); });
        return;
      }
      var type = pickType();
      var recorder = new MediaRecorder(stream, type ? { mimeType: type } : undefined);
      var r = rec = { recorder: recorder, stream: stream, chunks: [], started: Date.now(), form: form, button: button };
      recorder.ondataavailable = function (e) { if (e.data.size > 0) r.chunks.push(e.data); };
      recorder.onstop = function () { finished(r); };
      recorder.start();
      button.setAttribute("aria-pressed", "true");
      r.timer = setInterval(function () {
        var secs = Math.round((Date.now() - r.started) / 1000);
        button.textContent = "■ Stop " + clock(secs);
        if (secs >= MAX_SECONDS) stop(r);
      }, 250);
      button.textContent = "■ Stop 0:00";
      // Still held down: a hold. Let go already (or a tap): it records until the next tap.
      status(form, press ? "Recording… let go to stop." : "Recording… tap Stop when you're done.");
    }).catch(function () {
      asking = false;
      status(form, "Microphone not allowed. Check the browser's permission for this site.");
    });
  }

  function stop(r) {
    r = r || rec;
    if (!r || r.recorder.state === "inactive") return;
    clearInterval(r.timer);
    r.recorder.stop();
    r.stream.getTracks().forEach(function (t) { t.stop(); });
  }

  // abandon drops the voice note in progress without sending anything: the microphone goes off, and a
  // request for its words is cancelled.
  function abandon() {
    var r = rec;
    if (!r) return;
    rec = null;
    r.abandoned = true;
    stop(r);
    if (r.abort) r.abort.abort();
  }
  // leftPage abandons the voice note when its form is no longer on the page (another page was opened).
  function leftPage() { if (rec && !rec.form.isConnected) abandon(); }
  document.addEventListener("htmx:afterSettle", leftPage);
  document.addEventListener("htmx:historyRestore", leftPage);
  window.addEventListener("pagehide", abandon);

  function finished(r) {
    if (r.abandoned || !r.form.isConnected) return; // its page is gone: nothing is kept or sent
    var blob = new Blob(r.chunks, { type: r.recorder.mimeType || "audio/webm" });
    var preview = r.form.querySelector("[data-voice-preview]");
    r.button.textContent = "🎙 Record";
    r.button.setAttribute("aria-pressed", "false");
    status(r.form, "");
    r.blob = blob;
    if (preview) {
      preview.hidden = false;
      var audio = preview.querySelector("audio");
      if (audio.src) URL.revokeObjectURL(audio.src);
      audio.src = URL.createObjectURL(blob);
    }
    if (r.form.dataset.transcribe && !offline) transcribe(r);
  }

  // showHeard offers the words as the speech service heard them (when the tidy-up changed them),
  // behind a small "As heard" toggle. "" hides it.
  function showHeard(form, raw) {
    var box = form.querySelector("[data-heard]");
    if (!box) return;
    var text = box.querySelector("[data-heard-text]"), toggle = box.querySelector("[data-heard-toggle]");
    box.hidden = !raw;
    text.textContent = raw;
    text.hidden = true;
    toggle.setAttribute("aria-expanded", "false");
  }

  // transcribe sends the recording to be turned into words, then puts the words in the box. Nothing
  // is kept on the server yet: the recording stays here, and goes with the message (with the signed
  // token from the answer) when the person presses Send.
  function transcribe(r) {
    var form = r.form, sendBtn = form.querySelector('[data-voice="send"]');
    var mark = String(Date.now()) + Math.random(); // marks this request, so a late answer can't clear a newer one
    form.dataset.transcribing = mark;
    r.abort = window.AbortController ? new AbortController() : null; // Discard cancels the request
    function answered() { if (form.dataset.transcribing === mark) delete form.dataset.transcribing; }
    if (sendBtn) sendBtn.hidden = true;
    status(form, "Turning your voice note into words…");
    var data = new FormData();
    data.append("voice", r.blob, "voice-note." + extension(r.blob));
    if (form.dataset.item) data.append("item", form.dataset.item);
    fetch(form.dataset.transcribe, { method: "POST", body: data, credentials: "same-origin", headers: { Accept: "application/json" },
      signal: r.abort ? r.abort.signal : undefined })
      .then(function (res) { return res.json(); })
      .then(function (ans) {
        answered();
        if (rec !== r) return; // discarded meanwhile
        if (ans.text && ans.token) {
          var input = document.createElement("input");
          input.type = "hidden";
          input.name = "voice_token";
          input.value = ans.token;
          form.appendChild(input);
          var box = form.querySelector("textarea"), before = box.value.trim();
          r.words = ans.text;
          box.value = before ? before + " " + ans.text : ans.text;
          box.focus();
          box.setSelectionRange(box.value.length, box.value.length);
          showHeard(form, ans.raw || "");
          status(form, ans.raw
            ? "Seyal tidied the words (“As heard” shows them as spoken). Check them, then press Send. The recording goes with them."
            : "Check the words and correct them if needed, then press Send. The recording goes with them.");
          return;
        }
        if (sendBtn) sendBtn.hidden = false;
        status(form, ans.note || "No words could be made. You can still send it as a voice note.");
      })
      .catch(function () {
        // No answer (connection, or the login ended): the recording is still here, to send as a voice note.
        answered();
        if (rec !== r) return; // discarded meanwhile (which also cancels the request)
        if (sendBtn) sendBtn.hidden = false;
        status(form, "Couldn't turn the voice note into words. Press Send voice note to send it as it is.");
      });
  }

  function discard(form) {
    var preview = form.querySelector("[data-voice-preview]");
    if (preview) {
      var audio = preview.querySelector("audio");
      if (audio.src) URL.revokeObjectURL(audio.src);
      audio.removeAttribute("src");
      preview.hidden = true;
      var sendBtn = preview.querySelector('[data-voice="send"]');
      if (sendBtn) sendBtn.hidden = false;
    }
    var token = tokenInput(form);
    if (token) token.remove();
    showHeard(form, "");
    delete form.dataset.transcribing; // a discarded voice note mustn't hold up Send or Record
    if (rec && rec.form === form) {
      if (rec.abort) rec.abort.abort();
      // Take out the words that came from this recording, if the person hasn't changed them.
      var box = form.querySelector("textarea");
      if (rec.words && box && box.value.slice(-rec.words.length) === rec.words) {
        box.value = box.value.slice(0, -rec.words.length).trim();
      }
      rec = null;
    }
    status(form, "");
  }

  // refused: Seyal can't vouch for the words any more (over an hour old, or Seyal restarted). The
  // recording stays, to send as a plain voice note. The words heard are taken out if the person
  // hasn't changed them (as Discard does): unchecked, they would go as typed text. Words the person
  // corrected or added stay in the box, and the status says they will go as typed text.
  function refused(form, r) {
    delete form.dataset.sending;
    var token = tokenInput(form);
    if (token) token.remove();
    showHeard(form, "");
    var box = form.querySelector("textarea"), kept = false;
    if (box && r.words) {
      if (box.value.slice(-r.words.length) === r.words) box.value = box.value.slice(0, -r.words.length).trim();
      else kept = true;
    }
    r.words = undefined;
    var sendBtn = form.querySelector('[data-voice="send"]');
    if (sendBtn) sendBtn.hidden = false;
    status(form, "Seyal can't check these words any more (over an hour old, or Seyal was restarted). " + (kept
      ? "Your words are still in the box: they will go as typed text, with the recording as a plain voice note. Check them, then press Send."
      : "Send the recording as a voice note, or discard it and record again."));
  }

  function send(form) {
    if (!rec || rec.form !== form || !rec.blob) return;
    if (offline) {
      status(form, "No connection. The voice note is kept; send it when the connection is back.");
      return;
    }
    if (form.dataset.sending) return;
    form.dataset.sending = "1";
    var r = rec;
    var data = new FormData(form); // with the voice token, when the words came from this recording
    data.append("voice", r.blob, "voice-note." + extension(r.blob));
    status(form, "Sending…");
    fetch(form.action, { method: "POST", body: data, credentials: "same-origin" })
      .then(function (res) {
        if (res.redirected) { window.location.href = res.url; return; }
        if (res.headers.get("Seyal-Voice") === "refused" && rec === r) { refused(form, r); return; }
        return res.text().then(function (html) {
          var doc = new DOMParser().parseFromString(html, "text/html");
          if (!res.ok && rec === r) {
            // Refused (too many files, too long, …): nothing was kept. Say why, and keep the form, the
            // recording and the words, so the person can fix the message and send it again.
            delete form.dataset.sending;
            var why = doc.querySelector(".banner.error");
            status(form, (why ? why.textContent.trim() : "Seyal couldn't send this (error " + res.status + ").") +
              " The voice note is kept: change the message, then press Send again.");
            return;
          }
          // Sent: show the returned page (Seyal's reply, or a question about the process).
          if (rec === r) rec = null;
          document.body.replaceWith(doc.body);
          if (window.htmx) htmx.process(document.body);
          settle();
        });
      })
      .catch(function () {
        delete form.dataset.sending;
        status(form, "Couldn't send. The voice note is kept; send it again when the connection is back.");
        goneOffline();
      });
  }

  // --- Hold to talk -------------------------------------------------------------------------
  // Press and hold Record to talk, let go to stop; a quick tap starts, and a second tap stops. This
  // works with a mouse, a finger and the keyboard (Space or Enter held on the focused button).
  // Pointer capture keeps the "let go" on the button even when the finger slides off it.

  var HOLD_MS = 400; // recording this long before letting go counts as a hold
  var press = null;  // the Record button is held down: {button, key}
  var quiet = 0;     // until then, clicks on Record are the press's own and are ignored

  function pressDown(button, key) {
    quiet = Date.now() + 1000;
    leftPage();
    if (rec && rec.recorder.state === "recording") { stop(); return; } // the second tap
    press = { button: button, key: key };
    start(button);
  }

  function pressUp() {
    quiet = Date.now() + 500;
    var p = press;
    press = null;
    if (!p || !rec || rec.button !== p.button || rec.recorder.state !== "recording") return;
    if (Date.now() - rec.started >= HOLD_MS) stop(); // a hold: let go to stop
    else status(rec.form, "Recording… tap Stop when you're done."); // a quick tap: keep recording
  }

  function recordButton(el) { return el && el.closest ? el.closest('[data-voice="record"]') : null; }

  document.addEventListener("pointerdown", function (e) {
    var b = recordButton(e.target);
    if (!b || (e.pointerType === "mouse" && e.button !== 0)) return;
    e.preventDefault(); // no text selection
    try { b.setPointerCapture(e.pointerId); } catch (_) { /* fine without it */ }
    pressDown(b, "");
  });
  document.addEventListener("pointerup", function () { if (press && !press.key) pressUp(); });
  document.addEventListener("pointercancel", function () { if (press && !press.key) pressUp(); });
  document.addEventListener("contextmenu", function (e) { if (recordButton(e.target)) e.preventDefault(); }); // long press on a phone
  document.addEventListener("keydown", function (e) {
    var b = recordButton(e.target);
    if (!b || (e.key !== " " && e.key !== "Enter")) return;
    e.preventDefault(); // the press handles it, not a click
    if (!e.repeat && !press) pressDown(b, e.key);
  });
  document.addEventListener("keyup", function (e) {
    if (!press || e.key !== press.key) return;
    e.preventDefault();
    pressUp();
  });
  window.addEventListener("blur", function () { if (press) pressUp(); }); // e.g. another window took the keys

  document.addEventListener("click", function (e) {
    var t = e.target.closest("[data-heard-toggle]");
    if (!t) return;
    var text = t.parentNode.querySelector("[data-heard-text]");
    text.hidden = !text.hidden;
    t.setAttribute("aria-expanded", String(!text.hidden));
  });

  // --- Choosing what to start on Tell Seyal -------------------------------------------------
  // The process cards in the right pane belong to the message form (form="tell-form"). Searching
  // hides the cards that don't match; choosing one updates "Starting: …" and the example text.

  document.addEventListener("input", function (e) {
    if (!e.target.matches("[data-proc-search]")) return;
    var q = e.target.value.trim().toLowerCase(), shown = 0;
    document.querySelectorAll(".proc[data-search]").forEach(function (card) {
      var match = card.getAttribute("data-search").indexOf(q) >= 0;
      card.hidden = !match;
      if (match) shown++;
    });
    var none = document.querySelector("[data-proc-none]");
    if (none) none.hidden = shown > 0;
  });

  document.addEventListener("change", function (e) {
    if (!e.target.matches('.proc input[type="radio"]')) return;
    var chosen = document.querySelector("[data-chosen]");
    if (chosen) chosen.textContent = e.target.getAttribute("data-name");
    var example = e.target.getAttribute("data-example"), msg = document.getElementById("msg");
    if (msg && example) msg.placeholder = "For example: " + example;
  });

  // --- Connection band ----------------------------------------------------------------------
  // When the server can't be reached, a thin band says so and Seyal keeps checking; sending waits,
  // and whatever was typed or recorded stays on the page. When the server answers again, the band
  // says "Back online" for a moment. (Opening the app with no connection shows the offline page,
  // from the service worker.)

  var offline = false, checking = null, band = null;

  function showBand(text, kind) {
    if (!band) {
      band = document.createElement("div");
      band.className = "conn";
      band.setAttribute("role", "status");
      band.setAttribute("aria-live", "polite");
    }
    if (!band.isConnected) document.body.prepend(band); // htmx swaps the body on navigation
    band.className = "conn " + kind;
    band.textContent = text;
    if (kind === "down") {
      var b = document.createElement("button");
      b.type = "button";
      b.setAttribute("data-reload", "");
      b.textContent = "Refresh";
      band.append(" ", b);
    }
  }

  function hideBand() { if (band) band.remove(); }

  function goneOffline() {
    if (offline) return;
    offline = true;
    document.body.classList.add("is-offline");
    showBand("No connection · trying again…", "down");
    checking = setInterval(check, 5000);
  }

  function check() {
    fetch("/sw.js", { method: "HEAD", cache: "no-store" }).then(function (res) {
      if (res.ok) backOnline();
    }).catch(function () { /* still offline */ });
  }

  function backOnline() {
    if (!offline) return;
    offline = false;
    clearInterval(checking);
    document.body.classList.remove("is-offline");
    showBand("Back online", "up");
    setTimeout(function () { if (!offline) hideBand(); }, 2000);
  }

  window.addEventListener("offline", goneOffline);
  window.addEventListener("online", check);
  document.addEventListener("htmx:sendError", function () { // a link or form couldn't reach the server
    // What didn't arrive can be sent again later.
    document.querySelectorAll("form[data-sending]").forEach(function (f) { delete f.dataset.sending; });
    goneOffline();
  });

  // While offline, nothing is sent: the typed text stays in the box until the connection is back.
  function hold(e) {
    if (!offline) return;
    e.preventDefault();
    showBand("No connection · your message is kept; send it when the connection is back.", "down");
  }

  // Every form is sent once: a second click while the first is on its way is ignored (it would start
  // the same work twice). A recorded voice note goes with the message even if the person presses the
  // ordinary Send. Stopping the event here keeps htmx from sending it as well.
  document.addEventListener("submit", function (e) {
    var form = e.target;
    var stopHere = function () { e.preventDefault(); e.stopPropagation(); };
    if (offline) { hold(e); stopHere(); return; }
    if (form.dataset.sending) { stopHere(); return; }
    if (form.dataset.transcribing) {
      stopHere();
      status(form, "One moment: Seyal is still turning your voice note into words.");
      return;
    }
    if (rec && rec.form === form && rec.recorder.state === "recording") {
      // Sending now would leave the recording out (and the microphone on).
      stopHere();
      status(form, "Stop the recording first (tap Stop), then press Send.");
      return;
    }
    if (rec && rec.form === form && rec.blob) { stopHere(); send(form); return; }
    form.dataset.sending = "1"; // the page that comes back has a fresh form
  }, true);
  document.addEventListener("htmx:beforeRequest", function (e) {
    if (offline && e.detail.requestConfig && e.detail.requestConfig.verb !== "get") hold(e);
  });

  // --- Installable app ----------------------------------------------------------------------
  if ("serviceWorker" in navigator) {
    window.addEventListener("load", function () {
      navigator.serviceWorker.register("/sw.js").catch(function () { /* the app works without it */ });
    });
  }
  // --- Phone notes (web push) -----------------------------------------------------------------
  // Me's "Phone notes" block, and a short hello from every page, so Seyal knows this browser was
  // opened (a phone not opened for 30 days stops getting phone notes) and who is logged in on it (on a
  // shared phone, the previous person's phone notes end). The device push address goes only to Seyal's
  // own server and is never shown. Turning on (subscribe) is the one step that makes the browser ask
  // its push service for an address, and it happens only when the person taps Turn on.

  var pushOK = "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;
  var DEVICE = "seyal-push-device"; // this browser's device id, to end it if permission is taken back
  function keep(key, value) {
    try { if (value) localStorage.setItem(key, value); else localStorage.removeItem(key); } catch (_) { /* storage blocked */ }
  }
  function kept(key) {
    try { return localStorage.getItem(key); } catch (_) { return null; }
  }
  function pushSub() {
    if (!pushOK) return Promise.resolve(null);
    return navigator.serviceWorker.getRegistration("/")
      .then(function (reg) { return reg ? reg.pushManager.getSubscription() : null; })
      .catch(function () { return null; });
  }
  // postJSON sends to Seyal's phone-note routes. Only a JSON answer straight from the route counts:
  // a reply that was sent on somewhere else (to the password page, or the login page) is a page, not
  // the route's answer, so the route never saw the request and it is not ok.
  function postJSON(url, data) {
    return fetch(url, { method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) })
      .then(function (r) {
        var json = !r.redirected && /^application\/json(;|$)/.test(r.headers.get("Content-Type") || "");
        return (json ? r.json() : Promise.resolve({})).catch(function () { json = false; return {}; })
          .then(function (j) { j.ok = r.ok && json; return j; });
      });
  }
  // hello tells Seyal this browser was opened: at most once every 6 hours for one login, unless forced.
  function hello(force) {
    var url = document.body.getAttribute("data-hello"), login = document.body.getAttribute("data-login");
    if (!url) return Promise.resolve(null);
    return pushSub().then(function (sub) {
      if (!sub) return null;
      var mark = login + "|" + Math.floor(Date.now() / 21600000);
      if (!force && kept("seyal-push-hello") === mark) return null;
      return postJSON(url, sub.toJSON()).then(function (j) {
        if (j.ok) keep("seyal-push-hello", mark);
        return j;
      });
    });
  }
  function platform() {
    var ua = navigator.userAgent;
    if (/iPhone|iPad|iPod/.test(ua) || (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1)) return "iphone";
    if (/Android/.test(ua)) return "android";
    if (/Windows/.test(ua)) return "windows";
    return "";
  }
  function standalone() {
    return navigator.standalone === true || (window.matchMedia && window.matchMedia("(display-mode: standalone)").matches);
  }
  // ownApp: the page runs as one business's own app, with its own storage and its own push address:
  // an iPhone or iPad Home Screen icon, or a Mac Safari "Add to Dock" app (Safari's vendor is Apple;
  // Chrome, Edge and Firefox on a Mac share one address for all the businesses on this server).
  function ownApp() {
    return standalone() && (platform() === "iphone" || /^Apple/.test(navigator.vendor || ""));
  }
  function showPush(box, state, label) {
    box.querySelectorAll("[data-push-state]").forEach(function (el) { el.hidden = el.getAttribute("data-push-state") !== state; });
    if (label) box.querySelector("[data-push-label]").textContent = label;
    if (state === "blocked") {
      var os = platform();
      box.querySelectorAll("[data-push-os]").forEach(function (li) { li.hidden = os !== "" && li.getAttribute("data-push-os") !== os; });
    }
  }
  function pushMessage(box, text) {
    var m = box.querySelector("[data-push-msg]");
    m.textContent = text;
    m.hidden = !text;
  }
  // turnOffHere ends this browser's device on the server, and takes it out of the "Also on" list.
  function turnOffHere(box, why) {
    var id = box.dataset.device || kept(DEVICE);
    keep(DEVICE, "");
    if (!id) return Promise.resolve();
    box.querySelectorAll("[data-device]").forEach(function (row) {
      if (row.getAttribute("data-device") === id) row.hidden = true;
    });
    return fetch("/t/" + box.getAttribute("data-slug") + "/push/devices/" + encodeURIComponent(id) + "/off", {
      method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: "why=" + why,
    }).catch(function () { /* tried */ });
  }
  function pushBlock() {
    var box = document.getElementById("push-notes");
    if (!box || !box.getAttribute("data-key") || box.dataset.ready) return;
    box.dataset.ready = "1";
    var os = platform();
    if (os === "iphone" && !standalone()) { showPush(box, "iphone-tab"); return; }
    if (!pushOK) { showPush(box, os === "iphone" ? "iphone-old" : "unsupported"); return; }
    // The registration is fetched now, so a tap can subscribe at once (iOS needs it inside the tap).
    navigator.serviceWorker.ready.then(function (reg) { box.registration = reg; });
    if (Notification.permission === "denied") {
      // Permission was taken back (by the person, or by the browser's clean-up): end this device on
      // the server too, instead of saying "On" for a phone that gets nothing.
      turnOffHere(box, "blocked");
      showPush(box, "blocked");
      return;
    }
    pushSub().then(function (sub) {
      box.sub = sub; // a tap on Turn on drops it first (see turnOn)
      if (!sub || Notification.permission !== "granted") {
        // The browser lost its subscription (Chrome's clean-up for unused sites, or "Reset
        // permission", puts the permission back to "ask" and drops it): if this browser had notes
        // on, end that device too, so Me doesn't list this same phone under "Also on".
        if (kept(DEVICE)) turnOffHere(box, "blocked");
        showPush(box, "off");
        return;
      }
      return hello(true).then(function (j) {
        if (j && j.state === "on") {
          box.dataset.device = j.device;
          keep(DEVICE, j.device);
          var row = box.querySelector('[data-device="' + j.device + '"]');
          if (row) row.hidden = true; // it is "this phone" above, not "also on"
          showPush(box, "on", j.label);
        } else {
          if (j && j.ok) keep(DEVICE, ""); // Seyal has no open device for this browser any more
          showPush(box, j && j.state === "ended" ? "ended" : "off");
        }
      });
    }).catch(function () { showPush(box, "off"); });
  }
  function keyBytes(key) {
    var s = atob(key.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((key.length + 3) % 4));
    var out = new Uint8Array(s.length);
    for (var i = 0; i < s.length; i++) out[i] = s.charCodeAt(i);
    return out;
  }
  // turnOn is only offered when Seyal says notes are not on here (Off, or Ended). A subscription the
  // browser still holds is then of no use: Seyal ended it, or it was made with an older server key
  // (after a key change the browser refuses a new key while the old subscription is there). So the
  // old one is dropped first and a fresh one made, with a new address. With no old one, subscribe
  // runs at once, still inside the tap (iOS asks for the permission only from a tap); after an
  // unsubscribe it runs on the same registration in the same chain, which iOS still accepts.
  function turnOn(box, button) {
    pushMessage(box, "");
    if (!box.registration) { pushMessage(box, "One moment, then tap again."); return; }
    button.disabled = true;
    var reg = box.registration, old = box.sub;
    var subscribe = function () {
      return reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: keyBytes(box.getAttribute("data-key")) });
    };
    var made = old
      ? old.unsubscribe().catch(function () { /* try a new one anyway */ }).then(function () { box.sub = null; return subscribe(); })
      : subscribe();
    made
      .then(function (sub) {
        var body = sub.toJSON();
        // An iPhone Home Screen icon (or a Mac Dock app) is this business's own app, with its own
        // address: only this business sends to it, so a person with one per business doesn't get
        // each note twice.
        if (ownApp()) body.home = true;
        return postJSON("/t/" + box.getAttribute("data-slug") + "/push/devices", body);
      })
      .then(function (j) {
        if (!j.ok) { var err = new Error(j.error || ""); err.seyal = true; throw err; }
        keep(DEVICE, j.device);
        location.reload();
      })
      .catch(function (e) {
        button.disabled = false;
        if (Notification.permission === "denied") { showPush(box, "blocked"); return; }
        // Seyal's own answer is plain words; a browser's error text is not, so it is never shown.
        pushMessage(box, (e && e.seyal && e.message) || "Phone notes couldn't be turned on. Please try again.");
      });
  }
  function turnOff(box, button) {
    button.disabled = true;
    pushSub().then(function (sub) { return sub && sub.unsubscribe(); }).catch(function () { /* still end it on the server */ })
      .then(function () { return turnOffHere(box, "turned_off"); })
      .then(function () { location.reload(); });
  }
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-push]");
    var box = b && b.closest("#push-notes");
    if (!box) return;
    if (b.getAttribute("data-push") === "on") turnOn(box, b);
    else turnOff(box, b);
  });
  document.addEventListener("DOMContentLoaded", function () { hello(false); pushBlock(); });
  document.addEventListener("htmx:afterSettle", pushBlock);
  // Logging out also unsubscribes this browser, so a shared phone keeps nothing of this person (the
  // server ends the device either way). A share record still waiting here is sent first, then cleared:
  // it isn't kept past a log out (D91). The form is sent after that, or after 2 seconds at most.
  document.addEventListener("submit", function (e) {
    var form = e.target;
    var logout = /\/logout(-everywhere)?$/.test(form.getAttribute("action") || "");
    if (!logout || form.dataset.unsubscribed) return;
    var waiting = shareQueue().length > 0;
    if (!pushOK && !waiting) return;
    e.preventDefault();
    e.stopImmediatePropagation();
    form.dataset.unsubscribed = "1";
    var sent = false;
    var go = function () {
      if (sent) return;
      sent = true;
      keep(SHARES, "");
      if (pushOK) {
        keep(DEVICE, "");
        keep("seyal-push-hello", "");
      }
      HTMLFormElement.prototype.submit.call(form);
    };
    setTimeout(go, 2000);
    Promise.all([
      waiting ? sendShares() : null,
      pushOK ? pushSub().then(function (sub) { return sub && sub.unsubscribe(); }).catch(function () { /* logged out anyway */ }) : null
    ]).then(go, go);
  }, true);

  // --- Sending a set-your-password link -------------------------------------------------------
  // "Copy message" puts the message (with the link) on the clipboard; "Share…" opens the phone's
  // share sheet (WhatsApp and others) where the browser has one. Tapping the link box selects it.

  function showShare() {
    if (!navigator.share) return;
    document.querySelectorAll("[data-share][hidden]").forEach(function (b) { b.hidden = false; });
  }
  document.addEventListener("DOMContentLoaded", showShare);
  document.addEventListener("htmx:afterSettle", showShare);

  // copied shows on the button whether it worked; failed is what to do instead (default: the link).
  function copied(button, ok, failed) {
    var label = button.textContent;
    button.textContent = ok ? "Copied ✓" : (failed || "Select the link and copy it");
    setTimeout(function () { button.textContent = label; }, 2500);
  }
  // copyText copies text and shows it on button; done, if given, hears whether it worked.
  function copyText(text, button, done, failed) {
    var after = function (ok) { copied(button, ok, failed); if (done) done(ok); };
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(function () { after(true); }, function () { after(false); });
      return;
    }
    // Plain http (e.g. trying it on the local network): the older way.
    var area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    var ok = false;
    try { ok = document.execCommand("copy"); } catch (_) { /* not allowed */ }
    area.remove();
    after(ok);
  }
  document.addEventListener("click", function (e) {
    var copy = e.target.closest("[data-copy]");
    if (copy) copyText(copy.getAttribute("data-copy"), copy);
    var share = e.target.closest("[data-share]");
    if (share && navigator.share) navigator.share({ text: share.getAttribute("data-share") }).catch(function () { /* closed */ });
    var box = e.target.closest("[data-select]");
    if (box) box.select();
  });

  // --- Share (D91) ----------------------------------------------------------------------------------
  // The Work Item's Share panel: pick a message, check (or change) the words, then hand them to an app
  // on this device: an email, the clipboard, the phone's own share menu (More…) or, when the server
  // switched it on, a WhatsApp link (D92). Seyal's server sends nothing. After the hand-over the page reports it (POST …/shared), so the thread can say "Asha copied
  // ‘Tell a colleague’". The report carries the message's signed offer (data-offer), so Seyal judges it
  // by what it offered when the page was drawn, even if the item has moved on since. The report is kept
  // on this device first (localStorage) and sent with keepalive, which survives the page changing; if the
  // phone is offline or Seyal is down, it goes the next time a page of the same business opens here, for
  // the same login only (data-sharer, the same in all its sessions), for 7 days at most. Each different
  // hand-over in one opening of the panel (message, way, words) has its own attempt id, so a double tap
  // is recorded once.

  var SHARES = "seyal-share-records";
  var SHARE_MAX = 4000; // the most letters a share may hold (workitem.MaxShareText)
  var SHARE_LIFE = 7 * 24 * 3600 * 1000; // a record waits no longer (server/share.go offerLife)

  function uuid() {
    if (window.crypto && crypto.randomUUID) return crypto.randomUUID();
    var b = crypto.getRandomValues(new Uint8Array(16)); // plain http has no randomUUID
    b[6] = (b[6] & 15) | 64;
    b[8] = (b[8] & 63) | 128;
    var h = Array.prototype.map.call(b, function (x) { return (x + 256).toString(16).slice(1); }).join("");
    return h.slice(0, 8) + "-" + h.slice(8, 12) + "-" + h.slice(12, 16) + "-" + h.slice(16, 20) + "-" + h.slice(20);
  }
  // shareQueue is the records waiting on this device, less any older than SHARE_LIFE.
  function shareQueue() {
    var list;
    try { list = JSON.parse(kept(SHARES) || "[]") || []; } catch (_) { return []; }
    return list.filter(function (r) { return r && r.at && Date.now() - r.at < SHARE_LIFE; });
  }
  function keepShares(list) { keep(SHARES, list.length ? JSON.stringify(list) : ""); }
  function dropShare(attempt) { keepShares(shareQueue().filter(function (r) { return r.attempt !== attempt; })); }
  // shareHere is the start of this business's addresses ("/t/acme/"), from the page; "" on a page
  // outside a business.
  function shareHere() {
    var m = /^\/t\/[^/]+\//.exec(document.body.getAttribute("data-hello") || "");
    return m ? m[0] : "";
  }
  // letters counts as the server does (one per character, not per UTF-16 unit).
  function letters(text) { return Array.from(text).length; }

  function sharePicked(panel) { return panel.querySelector('input[name="share-msg"]:checked'); }
  function shareWords(panel) { return panel.querySelector("[data-share-box]").value; }
  function shareStatus(panel, text) { panel.querySelector("[data-share-status]").textContent = text; }
  // shareLinks is the picked message's WhatsApp and Email links for the words in the box (links.js,
  // D92): to the contact's phone and email when the message names one, each null or "" when the
  // finished link is too long. The WhatsApp link exists only when the server switched it on.
  function shareLinks(panel) {
    var m = sharePicked(panel);
    return SeyalLinks.buttons({ chatLinks: panel.hasAttribute("data-chat-links"), phone: m.getAttribute("data-phone") || "",
      email: m.getAttribute("data-email") || "", text: shareWords(panel),
      subject: m.getAttribute("data-label") + " · " + panel.getAttribute("data-number") });
  }
  // shareLength hides WhatsApp and Email when their finished link, counted after encoding, is too long
  // (and says so), and says so when the words are over 4,000 letters (a long detail can make even
  // Seyal's own words that long).
  function shareLength(panel) {
    var over = letters(shareWords(panel)) > SHARE_MAX, links = shareLinks(panel);
    var mailLong = !over && !links.mail;
    var chat = panel.querySelector('[data-share-route="chat_link"]');
    var chatLong = !!chat && !over && !links.chat;
    panel.querySelector('[data-share-route="email"]').hidden = mailLong || over;
    if (chat) chat.hidden = chatLong || over;
    panel.querySelector("[data-share-chat-note]").hidden = !chat || chat.hidden;
    var web = panel.querySelector("[data-share-chat-web]");
    if (web) web.hidden = true; // its link holds the words as they were when WhatsApp was tapped
    panel.querySelector("[data-share-chat-long]").hidden = !chatLong;
    panel.querySelector("[data-share-long]").hidden = !mailLong;
    panel.querySelector("[data-share-over]").hidden = !over;
  }
  // mobile is a phone or tablet: an iPad's Safari says "Macintosh", but has a touch screen.
  function mobile() {
    return /Android|iPhone|iPad|iPod/i.test(navigator.userAgent) || (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);
  }
  // openChat opens a WhatsApp link. On a phone or tablet it tries the app's own address, which skips
  // the website, and then shows a "didn't open?" link to the website. It never moves to the website on
  // its own: while the phone asks "Open in WhatsApp?" the page still shows, so a timer would send the
  // words to whatsapp.com (and could open the chat twice). The website is reached only by a second,
  // deliberate tap. On a computer it opens the website link (WhatsApp Web or the desktop app) in a new
  // tab.
  function openChat(panel, c) {
    if (!mobile()) { window.open(c.web, "_blank", "noopener"); return; }
    var web = panel.querySelector("[data-share-chat-web]");
    if (web) {
      web.querySelector("a").href = c.web;
      web.hidden = false;
    }
    location.href = c.app;
  }
  // shareFill puts the picked message's words in the box, with its hints: the words the person already
  // changed for that message in this panel, or else Seyal's.
  function shareFill(panel) {
    var m = sharePicked(panel);
    if (!m) return;
    var changed = Object.prototype.hasOwnProperty.call(panel.edits, m.value);
    panel.querySelector("[data-share-box]").value = changed ? panel.edits[m.value] : m.getAttribute("data-text");
    var n = +m.getAttribute("data-empty"), empty = panel.querySelector("[data-share-empty]");
    empty.textContent = n === 1 ? "1 detail is empty." : n + " details are empty.";
    empty.hidden = !n;
    panel.querySelectorAll("[data-share-for]").forEach(function (p) { p.hidden = p.getAttribute("data-share-for") !== m.getAttribute("data-to"); });
    shareStatus(panel, "");
    shareLength(panel);
  }
  function sharePanels() {
    document.querySelectorAll("[data-share-panel]").forEach(function (panel) {
      if (panel.dataset.ready) return;
      panel.dataset.ready = "1";
      panel.hidden = false;
      panel.querySelector('[data-share-route="menu"]').hidden = !navigator.share;
      panel.attempts = {};
      panel.edits = {}; // message id → the words the person changed, kept when they pick another message
      shareFill(panel);
    });
  }
  // sendShares sends the records waiting on this device (and extra, the one just made, even when the
  // browser keeps no storage). A record goes only with the login that made it: on a shared phone it is
  // never sent as someone else, and another login's records are dropped. It goes only from a page of
  // its own business, so sending it never opens that business behind the person's back. Seyal's answer
  // is final for a refusal or a bad record (dropped); with no answer, a server error or "too many", it
  // waits. It returns a promise that settles when every answer is in.
  function sendShares(extra, panel) {
    var sharer = document.body.getAttribute("data-sharer");
    var list = shareQueue();
    keepShares(list); // records too old to count go
    if (!sharer) return Promise.resolve(); // the login page: wait for the login
    if (extra && !list.some(function (r) { return r.attempt === extra.attempt; })) list.push(extra);
    var mine = list.filter(function (r) { return r.sharer === sharer; });
    if (mine.length !== list.length) keepShares(mine);
    var here = shareHere();
    return Promise.all(mine.filter(function (r) { return here && r.url.indexOf(here) === 0; }).map(function (r) {
      var own = panel && extra && r.attempt === extra.attempt;
      return fetch(r.url, { method: "POST", credentials: "same-origin", keepalive: true,
        body: new URLSearchParams({ share: r.share, route: r.route, text: r.text, attempt: r.attempt, offer: r.offer || "" }) })
        .then(function (resp) {
          var json = !resp.redirected && /^application\/json(;|$)/.test(resp.headers.get("Content-Type") || "");
          if (!json || resp.status >= 500 || resp.status === 429) {
            if (own) shareStatus(panel, "Not noted yet: Seyal notes it the next time it opens here.");
            return;
          }
          dropShare(r.attempt);
          if (!own) return;
          if (resp.ok) shareStatus(panel, "Noted in this item's thread.");
          else resp.json().then(function (j) { shareStatus(panel, j.error || "Seyal couldn't note it."); }, function () { /* no words */ });
        })
        .catch(function () { if (own) shareStatus(panel, "Not noted yet: Seyal notes it the next time it opens here."); });
    }));
  }
  function recordShare(panel, m, route, text) {
    var key = m.value + "|" + route + "|" + text;
    var attempt = panel.attempts[key] || (panel.attempts[key] = uuid());
    var rec = { url: panel.getAttribute("data-shared"), share: m.value, route: route, text: text, attempt: attempt,
      offer: m.getAttribute("data-offer") || "", sharer: document.body.getAttribute("data-sharer") || "", at: Date.now() };
    keepShares(shareQueue().filter(function (r) { return r.attempt !== attempt; }).concat([rec]).slice(-20));
    sendShares(rec, panel);
  }
  document.addEventListener("DOMContentLoaded", function () { sharePanels(); sendShares(); });
  document.addEventListener("htmx:afterSettle", function () { sharePanels(); sendShares(); });
  // Each opening of the panel starts new attempts ("toggle" doesn't bubble, so it is caught on the way down).
  document.addEventListener("toggle", function (e) {
    var t = e.target;
    if (t.matches && t.matches("[data-share-panel]") && t.open) t.attempts = {};
  }, true);
  document.addEventListener("change", function (e) {
    var panel = e.target.closest && e.target.closest("[data-share-panel]");
    if (panel && e.target.name === "share-msg") shareFill(panel);
  });
  document.addEventListener("input", function (e) {
    var panel = e.target.closest && e.target.closest("[data-share-panel]");
    if (!panel || !e.target.hasAttribute("data-share-box")) return;
    var m = sharePicked(panel);
    if (m) panel.edits[m.value] = e.target.value;
    shareLength(panel);
  });
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-share-route]");
    var panel = b && b.closest("[data-share-panel]");
    if (!panel) return;
    var m = sharePicked(panel), text = shareWords(panel), route = b.getAttribute("data-share-route");
    if (!m || !text.trim()) { shareStatus(panel, "Write the message first."); return; }
    // Nothing leaves that Seyal couldn't note: the server takes 4,000 letters at most.
    if (letters(text) > SHARE_MAX) { shareStatus(panel, "Too long: shorten it to 4,000 letters."); return; }
    var done = function () { recordShare(panel, m, route, text); };
    switch (route) {
      case "copy": // recorded once it is on the clipboard
        copyText(text, b, function (ok) { if (ok) done(); }, "Select the words and copy them");
        break;
      case "email": // recorded on the tap: Seyal can't see the email app
        var mail = shareLinks(panel).mail;
        if (!mail) { shareStatus(panel, "Too long for an email link: use More… or Copy."); return; }
        done();
        location.href = mail;
        break;
      case "chat_link": // recorded on the tap: Seyal can't see whether the person pressed Send
        var chat = shareLinks(panel).chat;
        if (!chat) { shareStatus(panel, "Too long for a WhatsApp link: use More… or Copy."); return; }
        done();
        openChat(panel, chat);
        break;
      case "menu": // recorded when the menu says an app took it; closing the menu records nothing
        if (!navigator.share) { shareStatus(panel, "This browser has no share menu. Use Copy instead."); return; }
        navigator.share({ text: text }).then(done, function (err) {
          if (!err || err.name !== "AbortError") shareStatus(panel, "The share menu didn't open here. Use Copy instead.");
        });
        break;
    }
  });

  document.addEventListener("click", function (e) {
    if (e.target.closest("[data-reload]")) location.reload();
    if (e.target.closest("[data-print]")) window.print(); // the browser offers "Save as PDF"
  });

  // A message spoken and then corrected shows its text with a small play button for the original.
  var playing = null; // {audio, button}
  function showPlaying(b, on) {
    b.setAttribute("aria-pressed", String(on));
    b.textContent = on ? "❚❚" : "▶";
  }
  // Going to another page (htmx swaps the page in place) or leaving stops the sound.
  function stopPlaying() {
    if (playing && !playing.button.isConnected) { playing.audio.pause(); playing = null; }
  }
  document.addEventListener("htmx:afterSettle", stopPlaying);
  document.addEventListener("htmx:historyRestore", stopPlaying);
  window.addEventListener("pagehide", function () { if (playing) { playing.audio.pause(); playing = null; } });
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-play]");
    if (!b) return;
    if (playing && playing.button === b && b.isConnected) {
      if (playing.audio.paused) { playing.audio.play(); showPlaying(b, true); }
      else { playing.audio.pause(); showPlaying(b, false); }
      return;
    }
    if (playing) { playing.audio.pause(); showPlaying(playing.button, false); }
    var audio = new Audio(b.getAttribute("data-play"));
    playing = { audio: audio, button: b };
    audio.onended = function () { showPlaying(b, false); };
    audio.play().catch(function () { showPlaying(b, false); });
    showPlaying(b, true);
  });

  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-voice]");
    if (!b) return;
    e.preventDefault();
    var what = b.getAttribute("data-voice");
    if (what === "record") {
      if (Date.now() < quiet) return; // already handled as a press (hold to talk)
      leftPage();
      rec && rec.recorder.state === "recording" ? stop() : start(b); // e.g. a screen reader's click
    }
    else if (what === "send") send(b.form);
    else if (what === "discard") discard(b.form);
  });
})();
