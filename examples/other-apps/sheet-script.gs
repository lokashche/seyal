/** @OnlyCurrentDoc */
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Seyal's sample script for a Google Sheet (Phase 4, part 4c-4). Paste it into the sheet's own
// Extensions -> Apps Script, set the script property SEYAL_SECRET, and deploy it as a web app.
// The guide is README.md next to this file.
//
// What it does: Seyal posts an item's details (signed with the secret); the script checks the
// signature, then adds one row to the TAB tab, or updates the row that already has the item's
// number, so a repeat never makes a second row. Test sends (the signed body says "test": true)
// go to TEST_TAB.
// A late retry of an older send never overwrites a newer one (it compares the send ids).
//
// Why it is safe:
// - "@OnlyCurrentDoc" (line 1): it can touch only this sheet, not the rest of the Google account.
// - There is no doGet, so nobody can read the sheet through the web app's address.
// - Every post must carry Seyal's signature and be under 5 minutes old; others are refused.
// - Data is read only from the signed part of the post.
// - A lock around "find the number, then add or update" stops two posts at once making two rows.
//   The writes are saved (SpreadsheetApp.flush) before the lock is let go.
// - The number in column A is always kept as text (a ' in front), so Sheets can't turn "0001"
//   into 1 or "2026-01" into a date, and a repeat finds its row again.
// - It checks the header row, so it never writes into the wrong columns.
// - Text that a sheet would read as a formula (=, +, -, @, tab, carriage return) gets a ' in front.
// - It always answers JSON: {"ok":true,"row":12}, or {"ok":false,"why":"..."} for something an
//   admin must fix. A busy sheet answers {"retry":true,...} (no "ok"), and Seyal tries again later.

// Change these three to match your sheet. HEADERS must be row 1 of TAB, in this order, and must
// match the "fields" of the process's "do: send" line (see README.md).
var TAB = 'Jobs';
var TEST_TAB = 'Seyal test';
var HEADERS = ['Number', 'Client', 'Nature of work', 'Fee', 'Currency', 'Advance', 'Timeline (days)', 'State', 'Send id'];

var MAX_AGE_SECONDS = 5 * 60; // refuse posts signed more than 5 minutes ago (or ahead)
var SIG_MARK = ',"sig":';

function doPost(e) {
  try {
    var post = (e && e.postData && e.postData.contents) || '';
    var signed = checkSignature(post);
    if (signed.why) return answer({ ok: false, why: signed.why });
    var data = signed.body.data || {};
    var row = data.row;
    if (!Array.isArray(row) || row.length === 0 || row[0] !== data.number) {
      return answer({ ok: false, why: 'the post has no row for the sheet' });
    }
    for (var i = 0; i < row.length; i++) {
      var cell = row[i];
      if (cell !== null && typeof cell !== 'string' && typeof cell !== 'number') {
        return answer({ ok: false, why: 'the row has a value that is not text or a number' });
      }
      row[i] = guard(cell === null ? '' : cell);
    }
    var lock = LockService.getScriptLock();
    if (!lock.tryLock(10000)) return answer({ retry: true, why: 'the sheet was busy' });
    try {
      // Only the signed "test": true marks a test send. Never the number: a business may number its
      // items TEST-..., and those real rows must still reach TAB.
      var result = signed.body.test === true ? writeTest(row) : writeRow(row, data.number, signed.body.id);
      SpreadsheetApp.flush(); // save the writes while the lock is still held
      return answer(result);
    } finally {
      lock.releaseLock();
    }
  } catch (err) {
    // Something unexpected (often Google's own services for a moment): no "ok", so Seyal tries again.
    return answer({ retry: true, why: 'the script stopped: ' + String(err && err.message).slice(0, 80) });
  }
}

// checkSignature follows the steps in Seyal's internal/send/sign.go ("Signed"). It answers
// {body: <the signed data>} or {why: <why the post is refused>}.
function checkSignature(post) {
  var secret = String(PropertiesService.getScriptProperties().getProperty('SEYAL_SECRET') || '').trim();
  if (!secret || secret.indexOf('whsec_') !== 0) {
    return { why: 'the script property SEYAL_SECRET is missing or does not start with whsec_' };
  }
  // Read the secret's bytes now, so a badly pasted secret gets a plain answer, not hours of retries.
  var key;
  try {
    key = Utilities.base64Decode(secret.slice('whsec_'.length));
  } catch (err) {
    key = null;
  }
  if (!key || key.length < 24 || key.length > 64) {
    return { why: 'SEYAL_SECRET is not a valid whsec_ secret (copy it again)' };
  }
  // 1. The last ,"sig": splits the post: the text before it, with "}" put back, is what was signed.
  var at = post.lastIndexOf(SIG_MARK);
  var signedText, rest;
  if (at >= 0) {
    signedText = post.slice(0, at) + '}';
    rest = post.slice(at + SIG_MARK.length);
  } else if (post.indexOf('{"sig":') === 0) {
    signedText = '{}';
    rest = post.slice('{"sig":'.length);
  } else {
    return { why: 'the post is not signed' };
  }
  // 2. After it: one JSON object, then a single "}", and nothing more (not even a space).
  if (rest.slice(-1) !== '}' || rest.slice(-2, -1) !== '}') return { why: 'the signature is not where it should be' };
  var sig;
  try {
    sig = JSON.parse(rest.slice(0, -1));
  } catch (err) {
    return { why: 'the signature is not where it should be' };
  }
  if (!sig || typeof sig.id !== 'string' || typeof sig.timestamp !== 'string' || typeof sig.signature !== 'string') {
    return { why: 'the signature is incomplete' };
  }
  // 3. The HMAC of id.timestamp.signedText, with the secret's bytes as the key, and the age.
  var text = Utilities.newBlob('').setDataFromString(sig.id + '.' + sig.timestamp + '.' + signedText, 'UTF-8').getBytes();
  var want = 'v1,' + Utilities.base64Encode(Utilities.computeHmacSha256Signature(text, key));
  if (!sameText(want, sig.signature)) return { why: 'the signature does not match (check SEYAL_SECRET)' };
  var age = Math.abs(Date.now() / 1000 - Number(sig.timestamp));
  if (!(age <= MAX_AGE_SECONDS)) return { why: 'the post is more than 5 minutes old (check the clocks)' };
  // 4. The data comes only from the signed text.
  var body = JSON.parse(signedText);
  if (body.id !== sig.id) return { why: 'the signature is for another send' };
  return { body: body };
}

// writeRow adds the row to TAB, or updates the row that has the same number in column A. If that
// row came from a newer send (a higher count in its send id), it is left alone: the post is an
// older send's late retry.
function writeRow(row, number, sendID) {
  var sheet = SpreadsheetApp.getActiveSpreadsheet().getSheetByName(TAB);
  if (!sheet) return { ok: false, why: 'no tab called ' + TAB };
  var width = HEADERS.length;
  if (sheet.getLastColumn() < width) return { ok: false, why: "the header row doesn't match" };
  var top = sheet.getRange(1, 1, 1, sheet.getLastColumn()).getValues()[0];
  for (var c = 0; c < top.length; c++) {
    if (String(top[c]).trim() !== (c < width ? HEADERS[c] : '')) return { ok: false, why: "the header row doesn't match" };
  }
  if (row.length !== width) return { ok: false, why: 'the post has ' + row.length + ' values, the header row ' + width };
  // Compare what the cells show (text), never the value Sheets may have read into a number or date.
  // A cell never shows the ' in front (Seyal's guard puts one before a number like +0001), so take
  // it off the number too.
  var want = String(number).replace(/^'/, '');
  var at = 0;
  var last = sheet.getLastRow();
  if (last >= 2) {
    var numbers = sheet.getRange(2, 1, last - 1, 1).getDisplayValues();
    for (var r = 0; r < numbers.length; r++) {
      if (numbers[r][0] === want) {
        at = r + 2;
        break;
      }
    }
  }
  if (at && newer(sheet.getRange(at, width).getDisplayValue(), sendID)) {
    return { ok: true, row: at, skipped: 'a newer send is already in this row' };
  }
  if (!at) at = last + 1;
  // Keep the number as text: a ' in front (the guard may have put one there already).
  if (String(row[0]).charAt(0) !== "'") row[0] = "'" + row[0];
  sheet.getRange(at, 1, 1, width).setValues([row]);
  return { ok: true, row: at };
}

// newer says whether the send id in the sheet (snd_<business>_<count>) is a later send of the same
// business than sendID. Seyal counts each business's sends up from 1; Send again keeps the same id.
function newer(inSheet, sendID) {
  var a = /^(.*)_(\d+)$/.exec(String(inSheet));
  var b = /^(.*)_(\d+)$/.exec(String(sendID));
  return !!(a && b && a[1] === b[1] && Number(a[2]) > Number(b[2]));
}

// writeTest adds a test send's row to TEST_TAB (made if it isn't there), so the real tab stays clean.
function writeTest(row) {
  var book = SpreadsheetApp.getActiveSpreadsheet();
  var sheet = book.getSheetByName(TEST_TAB) || book.insertSheet(TEST_TAB);
  var at = sheet.getLastRow() + 1;
  sheet.getRange(at, 1, 1, row.length).setValues([row]);
  return { ok: true, row: at, test: true };
}

// guard puts a ' in front of text a sheet would read as a formula. Seyal does this too; this is
// the second check. Numbers stay numbers.
function guard(value) {
  if (typeof value === 'string' && value !== '' && '=+-@\t\r'.indexOf(value.charAt(0)) >= 0) return "'" + value;
  return value;
}

// sameText compares two texts in a time that doesn't depend on where they differ.
function sameText(a, b) {
  if (a.length !== b.length) return false;
  var diff = 0;
  for (var i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}

function answer(obj) {
  return ContentService.createTextOutput(JSON.stringify(obj)).setMimeType(ContentService.MimeType.JSON);
}
