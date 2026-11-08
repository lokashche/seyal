// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Runs examples/other-apps/sheet-script.gs under Node with small stand-ins for Google's services
// (Utilities, PropertiesService, LockService, SpreadsheetApp, ContentService). TestSheetScriptRuns
// (sheetscript_test.go) makes the signed posts with Seyal's own code and runs this:
//   node sheetscript.js <sheet-script.gs> <posts.json>
// It prints one PASS or FAIL line per case and exits 1 if any failed.
// Like Google's, the stand-in sheet reads text the way a person typing it would ("0001" becomes the
// number 1, "2026-01" a date; a ' in front keeps it text), and holds writes until SpreadsheetApp.flush()
// (or a read): letting go of the lock with writes not yet saved counts as a failure.
const fs = require('fs'), vm = require('vm'), crypto = require('crypto');
const [,, scriptPath, postsPath] = process.argv;
const { secret, posts } = JSON.parse(fs.readFileSync(postsPath, 'utf8'));
const signed = b => Array.from(b, x => (x > 127 ? x - 256 : x));
const buf = a => Buffer.from(a.map(x => (x < 0 ? x + 256 : x)));
let pending = [], unflushed = 0;
const flush = () => { pending.forEach(f => f()); pending = []; };
// typed is what a sheet stores for a value written by setValues.
function typed(x) {
  if (typeof x !== 'string') return x;
  if (x.startsWith("'")) return x.slice(1);
  if (/^\d+$/.test(x)) return Number(x);
  const d = /^(\d{4})-(\d{1,2})$/.exec(x);
  if (d) return new Date(Date.UTC(+d[1], +d[2] - 1, 1));
  return x;
}
const shown = x => (x instanceof Date ? (x.getUTCMonth() + 1) + '/' + x.getUTCDate() + '/' + x.getUTCFullYear() : String(x));
function makeSheet(name, rows) {
  const s = { name, cells: rows.map(r => r.slice()), formulas: 0 };
  s.getLastRow = () => (flush(), s.cells.length);
  s.getLastColumn = () => (flush(), Math.max(0, ...s.cells.map(r => r.length)));
  const read = (r, c, nr, nc) => (flush(), Array.from({ length: nr }, (_, i) => Array.from({ length: nc }, (_, j) => ((s.cells[r - 1 + i] || [])[c - 1 + j] ?? ''))));
  s.getRange = (r, c, nr = 1, nc = 1) => ({
    getValues: () => read(r, c, nr, nc),
    getDisplayValues: () => read(r, c, nr, nc).map(row => row.map(shown)),
    getDisplayValue: () => shown(read(r, c, 1, 1)[0][0]),
    setValues: v => pending.push(() => v.forEach((row, i) => row.forEach((x, j) => {
      while (s.cells.length < r + i) s.cells.push([]);
      if (typeof x === 'string' && x.startsWith('=')) s.formulas++;
      s.cells[r - 1 + i][c - 1 + j] = typed(x);
    }))),
  });
  return s;
}
let book, lockFree = true, props = { SEYAL_SECRET: secret };
const ctx = {
  Utilities: {
    base64Decode: s => signed(Buffer.from(s, 'base64')),
    base64Encode: a => buf(a).toString('base64'),
    newBlob: () => ({ setDataFromString: (s, cs) => ({ getBytes: () => signed(Buffer.from(s, cs === 'UTF-8' ? 'utf8' : 'latin1')) }) }),
    computeHmacSha256Signature: (v, k) => signed(crypto.createHmac('sha256', buf(k)).update(buf(v)).digest()),
  },
  PropertiesService: { getScriptProperties: () => ({ getProperty: k => props[k] ?? null }) },
  LockService: { getScriptLock: () => ({ tryLock: () => lockFree, releaseLock: () => { if (pending.length) unflushed++; } }) },
  SpreadsheetApp: { getActiveSpreadsheet: () => book, flush },
  ContentService: { MimeType: { JSON: 'json' }, createTextOutput: s => ({ setMimeType: () => JSON.parse(s) }) },
};
vm.createContext(ctx);
vm.runInContext(fs.readFileSync(scriptPath, 'utf8'), ctx);
const H = ctx.HEADERS;
function fresh(tabs) { const m = {}; for (const [n, rows] of Object.entries(tabs)) m[n] = makeSheet(n, rows);
  book = { tabs: m, getSheetByName: n => m[n] || null, insertSheet: n => (m[n] = makeSheet(n, [])) }; }
const post = p => { const got = ctx.doPost({ postData: { contents: p } }); flush(); return got; }; // the run's end saves too
let fails = 0;
function check(name, got, want) {
  const ok = Object.entries(want).every(([k, v]) => JSON.stringify(got[k]) === JSON.stringify(v));
  if (!ok) fails++;
  console.log((ok ? 'PASS ' : 'FAIL ') + name + ' -> ' + JSON.stringify(got));
}
fresh({ Jobs: [H.slice()] });
check('real send adds row 2', post(posts.real), { ok: true, row: 2 });
check('repeat updates row 2', post(posts.real), { ok: true, row: 2 });
console.log('     rows in Jobs:', book.tabs.Jobs.cells.length, '; row 2:', JSON.stringify(book.tabs.Jobs.cells[1]));
if (book.tabs.Jobs.cells.length !== 2 || book.tabs.Jobs.formulas !== 0) { fails++; console.log('FAIL row count or formula'); }
check('unguarded formula is guarded again', post(posts.unguarded), { ok: true, row: 2 });
if (book.tabs.Jobs.formulas !== 0 || book.tabs.Jobs.cells[1][1] !== '=HYPERLINK("x")') { fails++; console.log('FAIL guard', book.tabs.Jobs.formulas); } else console.log('PASS cell stays text: ' + book.tabs.Jobs.cells[1][1]);
check('test send goes to Seyal test tab', post(posts.test), { ok: true, row: 1, test: true });
console.log('     Seyal test tab:', JSON.stringify(book.tabs['Seyal test'].cells), '; Jobs rows', book.tabs.Jobs.cells.length);
check('old post refused', post(posts.old), { ok: false, why: 'the post is more than 5 minutes old (check the clocks)' });
check('other secret refused', post(posts.othersecret), { ok: false, why: 'the signature does not match (check SEYAL_SECRET)' });
const sw = posts.real.replace('"state":"approved"', '"state":"cancelled"');
if (sw === posts.real) { fails++; console.log('FAIL the change case changed nothing'); }
check('changed data refused', post(sw), { ok: false, why: 'the signature does not match (check SEYAL_SECRET)' });
const add = posts.real.slice(0, -1) + ',"data":{"number":"X-1","row":["X-1"]}}';
check('text after sig refused', post(add), { ok: false, why: 'the signature is not where it should be' });
check('space after refused', post(posts.real + ' '), { ok: false, why: 'the signature is not where it should be' });
check('unsigned refused', post('{"data":{}}'), { ok: false, why: 'the post is not signed' });
check('empty post refused', post(''), { ok: false, why: 'the post is not signed' });
check('empty signed body: signature good, no id', post(posts.empty), { ok: false, why: 'the signature is for another send' });
fresh({ Sheet1: [H.slice()] });
check('no Jobs tab', post(posts.real), { ok: false, why: 'no tab called Jobs' });
const moved = H.slice(); [moved[1], moved[2]] = [moved[2], moved[1]];
fresh({ Jobs: [moved] });
check('moved column refused', post(posts.real), { ok: false, why: "the header row doesn't match" });
fresh({ Jobs: [H.concat(['Notes'])] });
check('added column refused', post(posts.real), { ok: false, why: "the header row doesn't match" });
fresh({ Jobs: [H.slice()] }); lockFree = false;
check('busy sheet: no ok, retry', post(posts.real), { ok: undefined, retry: true, why: 'the sheet was busy' });
lockFree = true; props = {};
check('no secret set', post(posts.real), { ok: false, why: 'the script property SEYAL_SECRET is missing or does not start with whsec_' });
props = { SEYAL_SECRET: 'whsec_short' };
check('bad secret: plain answer, not a retry', post(posts.real), { ok: false, why: 'SEYAL_SECRET is not a valid whsec_ secret (copy it again)' });
props = { SEYAL_SECRET: ' ' + secret + '\n' };
check('secret pasted with spaces still works', post(posts.real), { ok: true, row: 2 });
props = { SEYAL_SECRET: secret };
// Numbers a sheet would read as a number or a date: a repeat still finds its row.
fresh({ Jobs: [H.slice()] });
check('number 0001 adds row 2', post(posts.zeros), { ok: true, row: 2 });
check('number 0001 repeat updates row 2', post(posts.zeros), { ok: true, row: 2 });
check('number 2026-01 adds row 3', post(posts.datey), { ok: true, row: 3 });
check('number 2026-01 repeat updates row 3', post(posts.datey), { ok: true, row: 3 });
check('number +0001 (guarded by Seyal) adds row 4', post(posts.plus), { ok: true, row: 4 });
check('number +0001 repeat updates row 4', post(posts.plus), { ok: true, row: 4 });
const J = book.tabs.Jobs.cells;
if (J[3][0] !== '+0001') { fails++; console.log('FAIL +0001 kept as text', JSON.stringify(J[3][0])); }
if (J.length !== 4 || J[1][0] !== '0001' || J[2][0] !== '2026-01') { fails++; console.log('FAIL numbers kept as text', JSON.stringify(J.map(r => r[0]))); } else console.log('PASS numbers kept as text: 0001, 2026-01, +0001');
// Only the signed "test": true marks a test: a real item numbered TEST-... goes to the real tab.
fresh({ Jobs: [H.slice()] });
check('real item numbered TEST-2026-0001 adds Jobs row 2', post(posts.testnumber), { ok: true, row: 2 });
check('its repeat updates Jobs row 2', post(posts.testnumber), { ok: true, row: 2 });
if (book.tabs['Seyal test'] || book.tabs.Jobs.cells.length !== 2) { fails++; console.log('FAIL real TEST- item went to the test tab or added a row'); }
// A late retry of an older send must not overwrite a newer send's row; Send again (same id) may.
fresh({ Jobs: [H.slice()] });
check('newer send adds row 2', post(posts.newer), { ok: true, row: 2 });
check('older send arriving late is skipped', post(posts.real), { ok: true, row: 2, skipped: 'a newer send is already in this row' });
const N = book.tabs.Jobs.cells[1];
if (N[3] !== 18000 || N[8] !== 'snd_acme-demo_2') { fails++; console.log('FAIL newer row overwritten', JSON.stringify(N)); } else console.log('PASS row keeps the newer send: ' + JSON.stringify(N));
check('same send again updates row 2', post(posts.newer), { ok: true, row: 2 });
if (book.tabs.Jobs.cells.length !== 2) { fails++; console.log('FAIL rows in Jobs', book.tabs.Jobs.cells.length); }
if (unflushed) { fails++; console.log('FAIL the lock was let go ' + unflushed + ' time(s) before the writes were saved (SpreadsheetApp.flush)'); } else console.log('PASS every write saved before the lock was let go');
console.log(fails ? fails + ' FAILED' : 'ALL PASS');
process.exit(fails ? 1 : 0);
