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
  // --- Sending a set-your-password link -------------------------------------------------------
  // "Copy message" puts the message (with the link) on the clipboard; "Share…" opens the phone's
  // share sheet (WhatsApp and others) where the browser has one. Tapping the link box selects it.

  function showShare() {
    if (!navigator.share) return;
    document.querySelectorAll("[data-share][hidden]").forEach(function (b) { b.hidden = false; });
  }
  document.addEventListener("DOMContentLoaded", showShare);
  document.addEventListener("htmx:afterSettle", showShare);

  function copied(button, ok) {
    var label = button.textContent;
    button.textContent = ok ? "Copied ✓" : "Select the link and copy it";
    setTimeout(function () { button.textContent = label; }, 2500);
  }
  function copyText(text, button) {
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(function () { copied(button, true); }, function () { copied(button, false); });
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
    copied(button, ok);
  }
  document.addEventListener("click", function (e) {
    var copy = e.target.closest("[data-copy]");
    if (copy) copyText(copy.getAttribute("data-copy"), copy);
    var share = e.target.closest("[data-share]");
    if (share && navigator.share) navigator.share({ text: share.getAttribute("data-share") }).catch(function () { /* closed */ });
    var box = e.target.closest("[data-select]");
    if (box) box.select();
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
