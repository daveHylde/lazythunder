// Run: node --test addon/background.test.js  (mocked messenger; no Thunderbird needed)
const test = require("node:test");
const assert = require("node:assert");

const calls = [];
const msg = (id, extra = {}) => ({
  id, date: new Date("2025-01-02T03:04:05Z"), author: "A <a@x>", subject: "Re: Hi", read: false,
  flagged: false, junk: false, folder: { accountId: "account1", path: "/INBOX", name: "Inbox", id: "account1://INBOX" },
  recipients: ["b@x"], ccList: [], headerMessageId: `m${id}@x`, ...extra,
});
const pages = { L1: { id: "L2", messages: [msg(3), msg(4)] }, L2: { id: "L3", messages: [msg(5)] } };
globalThis.messenger = {
  runtime: { getManifest: () => ({ version: "1.0.0" }) },
  messages: {
    query: async (q) => { calls.push(["query", q]); return { id: "L1", messages: [msg(1), msg(2)] }; },
    continueList: async (id) => pages[id],
    abortList: async (id) => calls.push(["abort", id]),
    get: async (id) => { if (id === 99) throw new Error("Message not found: 99"); return msg(id); },
    getRaw: async () => "Subject: Hi\r\nX-Y: \xc3\xa6\r\n\r\nbody",
    update: async (id, props) => calls.push(["update", id, props]),
  },
  folders: {
    getFolderInfo: async () => ({ unreadMessageCount: 7 }),
    markAsRead: async (f) => calls.push(["markAsRead", f]),
  },
  accounts: { list: async () => [{ id: "account1", identities: [{ id: "id1", email: "Me@x" }] }] },
  compose: {
    beginNew: async (_, d) => { calls.push(["beginNew", d]); return { id: 42 }; },
    saveMessage: async (tab, o) => calls.push(["save", tab, o]),
  },
  tabs: { remove: async (tab) => calls.push(["remove", tab]) },
  accountsAdmin: {
    createAccount: async (p) => { calls.push(["createAccount", p]); return { id: "account9", incoming: { ...p.incoming, username: "u" }, outgoing: null, identities: [] }; },
    deleteAccount: async () => { throw new Error("NOT_FOUND: no account account7"); },
    findConfig: async () => ({ incoming: { type: "imap", hostname: "imap.x", port: 993, socketType: 3, auth: 10, username: "me@x" }, outgoing: { type: "smtp", hostname: "smtp.x", port: 587, socketType: 1, auth: 4, username: "me@x" }, source: "isp" }),
  },
};
const bg = require("./background.js");

test("search stops at limit+1, sets hasMore, aborts the list", async () => {
  calls.length = 0;
  const r = await bg.handle({ id: 1, op: "search", params: { from: "a", since: "2025-01-01T00:00:00Z", limit: 3 } });
  assert.equal(r.ok, true);
  assert.deepEqual(r.data.messages.map((m) => m.id), [1, 2, 3]);
  assert.equal(r.data.hasMore, true);
  assert.equal(r.data.total, 3);
  const q = calls[0][1];
  assert.equal(q.author, "a");
  assert.equal(q.junk, false);
  assert.equal(q.messagesPerPage, 4);
  assert.ok(q.fromDate instanceof Date);
  assert.deepEqual(calls[1], ["abort", "L2"]);
  assert.equal(r.data.messages[0].date, "2025-01-02T03:04:05.000Z");
  assert.deepEqual(r.data.messages[0].folder, { accountId: "account1", path: "/INBOX", name: "Inbox" });
});

test("search exhausting all pages has no hasMore and no abort", async () => {
  calls.length = 0;
  const r = await bg.handle({ id: 2, op: "search", params: { includeJunk: true, limit: 10 } });
  assert.equal(r.data.messages.length, 5);
  assert.equal(r.data.hasMore, false);
  assert.ok(!calls.some((c) => c[0] === "abort"));
  assert.equal(calls[0][1].junk, undefined);
});

test("errors carry codes", async () => {
  assert.equal((await bg.handle({ id: 3, op: "nope" })).code, "UNKNOWN_OP");
  assert.equal((await bg.handle({ id: 4, op: "read", params: { id: "x" } })).code, "INVALID_ARGS");
  assert.equal((await bg.handle({ id: 5, op: "thread", params: { id: 99 } })).code, "NOT_FOUND");
  assert.equal((await bg.handle({ id: 6, op: "mark", params: { ids: [1], flag: "bogus" } })).code, "INVALID_ARGS");
});

test("mark, markFolderRead, headers", async () => {
  calls.length = 0;
  assert.equal((await bg.handle({ id: 7, op: "mark", params: { ids: [1, 2], flag: "not-junk" } })).ok, true);
  assert.deepEqual(calls, [["update", 1, { junk: false }], ["update", 2, { junk: false }]]);
  const r = await bg.handle({ id: 8, op: "markFolderRead", params: { folder: "account1://INBOX" } });
  assert.deepEqual(r.data, { marked: 7 });
  const h = await bg.handle({ id: 9, op: "headers", params: { id: 1 } });
  assert.equal(h.data.raw, "Subject: Hi\r\nX-Y: æ");
});

test("submit draft resolves identity, saves, closes the tab", async () => {
  calls.length = 0;
  const r = await bg.handle({ id: 10, op: "submit", params: { kind: "new", from: "me@X", to: "b@x", subject: "s", body: "hi", send: false } });
  assert.equal(r.ok, true, r.error);
  assert.deepEqual(calls, [
    ["beginNew", { to: ["b@x"], subject: "s", isPlainText: true, plainTextBody: "hi", identityId: "id1" }],
    ["save", 42, { mode: "draft" }],
    ["remove", 42],
  ]);
});

test("extractParts splits text, html, attachments", () => {
  const p = bg.extractParts({ contentType: "multipart/mixed", parts: [
    { contentType: "text/plain", body: "t", partName: "1.1" },
    { contentType: "text/html", body: "<b>h</b>", partName: "1.2" },
    { contentType: "application/pdf", name: "a.pdf", partName: "1.3", size: 9 },
  ] });
  assert.deepEqual(p, { text: "t", html: "<b>h</b>", attachments: [{ name: "a.pdf", contentType: "application/pdf", partName: "1.3", size: 9 }] });
});

test("thread ids from References / In-Reply-To / Message-ID", () => {
  const ids = bg.buildThreadIds("Message-ID: <c@x>\r\nReferences: <a@x>\r\n <b@x>\r\nIn-Reply-To: <b@x>\r\n\r\n");
  assert.deepEqual([...ids], ["a@x", "b@x", "c@x"]);
  assert.equal(bg.normalizeSubject("Re: WG: [list] Call"), "Call");
});

test("changed: leading emit, one trailing per window per folder", async () => {
  const sent = [];
  bg.setPort({ postMessage: (m) => sent.push(m) });
  for (let i = 0; i < 5; i++) bg.changed("f1");
  bg.changed("f2");
  assert.deepEqual(sent.map((m) => m.data.folder), ["f1", "f2"]);
  await new Promise((r) => setTimeout(r, 1100));
  assert.deepEqual(sent.map((m) => m.data.folder), ["f1", "f2", "f1"]);
  await new Promise((r) => setTimeout(r, 1100));
  assert.equal(sent.length, 3);
  assert.equal(sent[0].event, "changed");
});

test("account servers map protocol strings to Thunderbird enums and back", async () => {
  calls.length = 0;
  const server = (type, port) => ({ type, hostname: `${type}.x`, port, socketType: "tls", auth: "password-cleartext", username: "me" });
  const r = await bg.handle({ id: 1, op: "createAccount", params: { name: "Me", email: "me@x", password: "pw", incoming: server("imap", 993), outgoing: server("smtp", 465) } });
  assert.equal(r.ok, true, r.error);
  const sent = calls[0][1];
  assert.deepEqual([sent.incoming.socketType, sent.incoming.auth, sent.outgoing.type], [3, 3, "smtp"]);
  assert.deepEqual([r.data.incoming.socketType, r.data.incoming.auth, r.data.outgoing], ["tls", "password-cleartext", null]);

  const f = await bg.handle({ id: 2, op: "findConfig", params: { email: "me@x" } });
  assert.deepEqual([f.data.incoming.auth, f.data.outgoing.socketType, f.data.outgoing.auth], ["oauth2", "starttls-optional", "password-encrypted"]);
});

test("account ops validate input and keep the experiment's error code", async () => {
  const bad = await bg.handle({ id: 1, op: "createAccount", params: { name: "Me", email: "me@x", incoming: { type: "imap", hostname: "h", port: 70000, socketType: "tls", auth: "oauth2" }, outgoing: {} } });
  assert.equal(bad.code, "INVALID_ARGS");
  assert.match(bad.error, /incoming.port/);
  const partial = bg.toBackendServer({ port: 143 }, "incoming", false);
  assert.deepEqual(partial, { port: 143 });
  assert.throws(() => bg.toBackendServer({ type: "smtp" }, "incoming", false), /incoming.type/);
  const gone = await bg.handle({ id: 2, op: "deleteAccount", params: { id: "account7", removeFiles: true } });
  assert.deepEqual([gone.code, gone.error], ["NOT_FOUND", "no account account7"]);
});
