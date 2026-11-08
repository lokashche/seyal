// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Seyal's share links (D92): the WhatsApp and Email links the Share panel builds from the words in its
// box. Pure functions, with no page in them, so a test can run them in Node (web/links_test.go). app.js
// uses them for the buttons. Each link is counted after encoding: "₹" and each Tamil letter take 9
// characters in a link, so a short-looking text can make a long link.
var SeyalLinks = (function () {
  // LINK_MAX is the longest link Seyal offers: some email apps (Outlook on Windows) cut longer mailto:
  // links, and very long chat links fail on some phones. Over it, the button hides: "use More… or Copy".
  var LINK_MAX = 2000;

  // enc encodes one part of a link (RFC 3986 / RFC 6068: a space is %20, never "+").
  function enc(s) { return encodeURIComponent(s); }

  // chat is the chat link for text, to phone (digits only, from the server) or, without one, asking
  // the person which contact. app is tried first on a phone (it skips the website); web is the
  // fallback, and the only one on a computer.
  function chat(phone, text) {
    return {
      app: "whatsapp://send?" + (phone ? "phone=" + enc(phone) + "&" : "") + "text=" + enc(text),
      web: "https://wa.me/" + (phone ? enc(phone) : "") + "?text=" + enc(text)
    };
  }

  // mail is the mailto: link (RFC 6068), to email (a plain address from the server, or "") with the
  // subject and the words as its body. The address, subject and body are each encoded; "@" is kept as
  // it is, as RFC 6068 writes it.
  function mail(email, subject, body) {
    return "mailto:" + enc(email || "").replace(/%40/g, "@") + "?subject=" + enc(subject) + "&body=" + enc(body);
  }

  // buttons is what the Share panel offers for these words: each link, or "" when it is over LINK_MAX
  // (the button then hides). chat is "" too when chat links are switched off.
  function buttons(o) {
    var c = o.chatLinks ? chat(o.phone, o.text) : null;
    var m = mail(o.email, o.subject, o.text);
    return {
      chat: c && c.web.length <= LINK_MAX && c.app.length <= LINK_MAX ? c : null,
      mail: m.length <= LINK_MAX ? m : ""
    };
  }

  return { LINK_MAX: LINK_MAX, chat: chat, mail: mail, buttons: buttons };
})();
if (typeof module !== "undefined" && module.exports) module.exports = SeyalLinks;
