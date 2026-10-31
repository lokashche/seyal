// Seyal's only script of its own: keep the newest message in view, and let Ctrl/Cmd+Enter send.
(function () {
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
})();
