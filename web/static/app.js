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

  document.addEventListener("keydown", function (e) {
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey) && e.target.matches(".composer textarea")) {
      e.preventDefault();
      e.target.form.requestSubmit();
    }
  });

  // --- Voice notes ------------------------------------------------------------------------
  // Record with the browser's MediaRecorder, let the person listen back, then send the recording
  // with the composer form (plus any text typed). The server stores it like any other file.

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

  function start(button) {
    var form = button.form;
    if (!navigator.mediaDevices || !window.MediaRecorder) {
      status(form, "This browser can't record voice notes.");
      return;
    }
    navigator.mediaDevices.getUserMedia({ audio: true }).then(function (stream) {
      var type = pickType();
      var recorder = new MediaRecorder(stream, type ? { mimeType: type } : undefined);
      rec = { recorder: recorder, stream: stream, chunks: [], started: Date.now(), form: form, button: button };
      recorder.ondataavailable = function (e) { if (e.data.size > 0) rec.chunks.push(e.data); };
      recorder.onstop = function () { finished(); };
      recorder.start();
      button.setAttribute("aria-pressed", "true");
      rec.timer = setInterval(function () {
        var secs = Math.round((Date.now() - rec.started) / 1000);
        button.textContent = "■ Stop " + clock(secs);
        if (secs >= MAX_SECONDS) stop();
      }, 250);
      button.textContent = "■ Stop 0:00";
      status(form, "Recording…");
    }).catch(function () {
      status(form, "Microphone not allowed. Check the browser's permission for this site.");
    });
  }

  function stop() {
    if (!rec) return;
    clearInterval(rec.timer);
    rec.recorder.stop();
    rec.stream.getTracks().forEach(function (t) { t.stop(); });
  }

  function finished() {
    var r = rec;
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
  }

  function discard(form) {
    var preview = form.querySelector("[data-voice-preview]");
    if (preview) {
      var audio = preview.querySelector("audio");
      if (audio.src) URL.revokeObjectURL(audio.src);
      audio.removeAttribute("src");
      preview.hidden = true;
    }
    if (rec && rec.form === form) rec = null;
  }

  function send(form) {
    if (!rec || rec.form !== form || !rec.blob) return;
    var data = new FormData(form);
    var ext = rec.blob.type.indexOf("mp4") >= 0 ? "m4a" : rec.blob.type.indexOf("ogg") >= 0 ? "ogg" : "webm";
    data.append("voice", rec.blob, "voice-note." + ext);
    status(form, "Sending…");
    fetch(form.action, { method: "POST", body: data, credentials: "same-origin" })
      .then(function (res) {
        if (res.redirected) { window.location.href = res.url; return; }
        return res.text().then(function (html) {
          // Show the returned page, which explains what was wrong.
          var doc = new DOMParser().parseFromString(html, "text/html");
          document.body.replaceWith(doc.body);
          if (window.htmx) htmx.process(document.body);
          settle();
        });
      })
      .catch(function () { status(form, "Couldn't send. Check the connection and try again."); });
  }

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

  // --- Installable app ----------------------------------------------------------------------
  if ("serviceWorker" in navigator) {
    window.addEventListener("load", function () {
      navigator.serviceWorker.register("/sw.js").catch(function () { /* the app works without it */ });
    });
  }
  document.addEventListener("click", function (e) {
    if (e.target.closest("[data-reload]")) location.reload();
  });

  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-voice]");
    if (!b) return;
    e.preventDefault();
    var what = b.getAttribute("data-voice");
    if (what === "record") { rec && rec.recorder.state === "recording" ? stop() : start(b); }
    else if (what === "send") send(b.form);
    else if (what === "discard") discard(b.form);
  });
})();
