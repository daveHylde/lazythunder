/**
 * lazythunder add-on: serves lazythunder over native messaging (see PROTOCOL.md).
 *
 * Message formatting, MIME part extraction, paged collection, thread reconstruction
 * and the compose flow are adapted from thunderbird-cli's add-on
 * (https://github.com/vitalio-sh/thunderbird-cli, MIT, (c) Vitalii Ionov).
 */

const HOST = "lazythunder";
const RECONNECT_MS = 5000;
const CHANGED_DEBOUNCE_MS = 1000;
const IPC_CONCURRENCY = 8; // per-message messenger.* calls in flight across all requests
const BASE64_CHUNK = 0x8000;
const DEFAULT_LIMIT = 25;

const api = typeof messenger !== "undefined" ? messenger : (typeof browser !== "undefined" ? browser : null);

// ─── Native messaging ───────────────────────────────────────────────

let port = null;

function connect() {
  const p = api.runtime.connectNative(HOST);
  port = p;
  p.onMessage.addListener(async (req) => {
    const res = await handle(req);
    if (port === p && res) p.postMessage(res);
  });
  p.onDisconnect.addListener(() => {
    console.log("[lazythunder] disconnected:", p.error?.message || "host exited");
    if (port === p) port = null;
    setTimeout(connect, RECONNECT_MS);
  });
}

function emit(event, data) {
  if (port) port.postMessage({ event, data });
}

/** One request → one response envelope ({id, ok, data} or {id, ok:false, error, code}). */
async function handle(req) {
  if (!req || typeof req !== "object" || req.id === undefined) return null;
  try {
    const fn = OPS[req.op];
    if (!Object.hasOwn(OPS, req.op)) throw codeErr("UNKNOWN_OP", `unknown op: ${req.op}`);
    const params = req.params ?? {};
    if (typeof params !== "object" || Array.isArray(params)) throw invalid("params must be an object");
    const data = await fn(params);
    return { id: req.id, ok: true, data: data ?? null };
  } catch (e) {
    const error = e?.message || String(e);
    const code = e?.code || (/not found|invalid message id|no message/i.test(error) ? "NOT_FOUND" : "THUNDERBIRD_ERROR");
    return { id: req.id, ok: false, error, code };
  }
}

// ─── Ops ────────────────────────────────────────────────────────────

const OPS = {
  async health() {
    return { version: api.runtime.getManifest().version };
  },

  async accounts() {
    return (await api.accounts.list(false)).map((a) => ({
      id: a.id, name: a.name, type: a.type,
      identities: (a.identities || []).map((i) => ({ id: i.id, email: i.email, name: i.name })),
    }));
  },

  async folders() {
    const flat = [];
    const walk = (f, depth) => {
      flat.push({ f, depth });
      for (const sub of f.subFolders || []) walk(sub, depth + 1);
    };
    for (const a of await api.accounts.list(true)) if (a.rootFolder) walk(a.rootFolder, 0);
    return mapLimited(flat, async ({ f, depth }) => {
      // Account roots are containers; Thunderbird rejects count requests for them.
      const info = f.isRoot || f.path === "/" ? {} : await api.folders.getFolderInfo(folderId(f));
      return {
        id: folderId(f), accountId: f.accountId, name: f.name, path: f.path,
        type: f.type || f.specialUse?.[0] || "",
        unreadMessageCount: info.unreadMessageCount || 0,
        totalMessageCount: info.totalMessageCount || 0,
        depth,
      };
    });
  },

  async search(p) {
    const limit = p.limit ?? DEFAULT_LIMIT;
    if (!Number.isSafeInteger(limit) || limit < 0) throw invalid("limit must be a nonnegative integer");
    const q = { messagesPerPage: Math.min(Math.max(limit + 1, 1), 1000) };
    if (p.from) q.author = str(p.from, "from");
    if (p.subject) q.subject = str(p.subject, "subject");
    if (p.folder) q.folderId = str(p.folder, "folder");
    if (p.since) q.fromDate = date(p.since, "since");
    if (p.until) q.toDate = date(p.until, "until");
    if (p.unread) q.unread = true;
    if (!p.includeJunk) q.junk = false;
    const { messages, hasMore } = await collect(api.messages.query(q), limit);
    return { messages, total: messages.length, hasMore };
  },

  // Each lookup may name candidate folders: a query scoped to one folder costs
  // a scan of that folder (~40 ms for 28k messages), an unscoped one scans all.
  async byMessageId(p) {
    if (!Array.isArray(p.lookups)) throw invalid("lookups must be an array");
    const queries = p.lookups.flatMap((l) => {
      const q = { headerMessageId: stripAngle(str(l?.id, "lookups.id")) };
      const folders = Array.isArray(l.folders) && l.folders.length ? l.folders : [null];
      return folders.map((f) => (f ? { ...q, folderId: str(f, "lookups.folders") } : q));
    });
    const pages = await mapLimited(queries, async (q) => (await collect(api.messages.query(q), Infinity)).messages);
    return pages.flat();
  },

  async read(p) {
    const id = msgId(p.id);
    const [msg, full] = await Promise.all([api.messages.get(id), api.messages.getFull(id)]);
    if (!msg) throw codeErr("NOT_FOUND", `Message not found: ${id}`);
    return { ...formatMessage(msg), parts: extractParts(full) };
  },

  async headers(p) {
    return { raw: await rawHeaders(await api.messages.getRaw(msgId(p.id))) };
  },

  async thread(p) {
    const id = msgId(p.id);
    const msg = await api.messages.get(id);
    if (!msg) throw codeErr("NOT_FOUND", `Message not found: ${id}`);
    // getFull() does not reliably expose RFC 2822 headers; parse them from the raw source.
    let ids = new Set();
    try {
      ids = buildThreadIds(await rawHeaders(await api.messages.getRaw(id)));
    } catch { /* fall back to the header id and subject */ }
    if (msg.headerMessageId) ids.add(stripAngle(msg.headerMessageId));

    const seen = new Set();
    const thread = [];
    const add = (m) => { if (!seen.has(m.id)) { seen.add(m.id); thread.push(m); } };
    // Upstream: every message named in References / In-Reply-To.
    const pages = await mapLimited([...ids], async (hdr) =>
      (await collect(api.messages.query({ headerMessageId: hdr }), Infinity)).messages);
    pages.flat().forEach(add);
    // Downstream: replies that don't reference this one yet but share its normalized subject.
    // The subject query is a substring match, so keep exact matches only; junk excluded.
    const norm = normalizeSubject(msg.subject || "").toLowerCase();
    if (norm) {
      const r = await collect(api.messages.query({ subject: normalizeSubject(msg.subject), junk: false }), Infinity);
      for (const m of r.messages) if (normalizeSubject(m.subject || "").toLowerCase() === norm) add(m);
    }
    thread.sort((a, b) => new Date(a.date) - new Date(b.date));
    return { thread };
  },

  async attachments(p) {
    return (await api.messages.listAttachments(msgId(p.id))).map((a) => ({
      name: a.name, contentType: a.contentType, partName: a.partName, size: a.size,
      contentId: a.contentId || "",
    }));
  },

  async download(p) {
    const file = await api.messages.getAttachmentFile(msgId(p.id), str(p.part, "part"));
    return { data: bytesToBase64(new Uint8Array(await file.arrayBuffer())) };
  },

  async mark(p) {
    const props = FLAGS[p.flag];
    if (!props) throw invalid(`flag must be one of ${Object.keys(FLAGS).join(", ")}`);
    await mapLimited(msgIds(p.ids), (id) => api.messages.update(id, props));
  },

  async markFolderRead(p) {
    const folder = str(p.folder, "folder");
    const info = await api.folders.getFolderInfo(folder);
    await api.folders.markAsRead(folder);
    return { marked: info.unreadMessageCount || 0 };
  },

  async move(p) {
    await api.messages.move(msgIds(p.ids), str(p.folder, "folder"));
  },

  async archive(p) {
    await api.messages.archive(msgIds(p.ids));
  },

  async delete(p) {
    await api.messages.delete(msgIds(p.ids), false);
  },

  async submit(p) {
    const body = p.body ?? "";
    if (typeof body !== "string") throw invalid("body must be a string");
    const identityId = p.from ? await resolveIdentity(str(p.from, "from")) : undefined;
    const list = (v, name) => (v ? [str(v, name)] : undefined); // Thunderbird parses "a, b <c>" lists
    let tab;
    switch (p.kind) {
      case "new":
        tab = await api.compose.beginNew(null, prune({
          to: list(p.to, "to"), cc: list(p.cc, "cc"), subject: p.subject ?? "",
          isPlainText: true, plainTextBody: body, identityId,
        }));
        break;
      case "reply":
      case "replyAll": {
        const id = msgId(p.replyTo);
        // Open the native reply so Thunderbird builds the quotation, then prepend our text;
        // a body passed to beginReply would replace the quote.
        tab = await api.compose.beginReply(id, p.kind === "replyAll" ? "replyToAll" : "replyToSender", prune({ identityId }));
        try {
          await prependReply(tab.id, body);
        } catch (e) {
          await closeTab(tab.id);
          throw e;
        }
        break;
      }
      case "forward":
        tab = await api.compose.beginForward(msgId(p.replyTo), "forwardAsAttachment", prune({
          to: list(p.to, "to"), cc: list(p.cc, "cc"), isPlainText: true, plainTextBody: body, identityId,
        }));
        break;
      default:
        throw invalid("kind must be new, reply, replyAll or forward");
    }
    try {
      if (p.send === true) await api.compose.sendMessage(tab.id, { mode: "sendNow" });
      else await api.compose.saveMessage(tab.id, { mode: "draft" });
    } finally {
      await closeTab(tab.id); // headless: nobody can close a leftover compose window
    }
  },

  // ─── Account management (experiments/accountsAdmin) ───

  async accountDetails() {
    return (await admin().details()).map(fromBackendAccount);
  },

  async findConfig(p) {
    return fromBackendConfig(await adminCall(() => admin().findConfig(str(p.email, "email"))));
  },

  async createAccount(p) {
    const params = {
      name: str(p.name, "name"), email: str(p.email, "email"), password: p.password ?? "",
      incoming: toBackendServer(p.incoming, "incoming", true), outgoing: toBackendServer(p.outgoing, "outgoing", true),
    };
    if (typeof params.password !== "string") throw invalid("password must be a string");
    return fromBackendAccount(await adminCall(() => admin().createAccount(params)));
  },

  async deleteAccount(p) {
    await adminCall(() => admin().deleteAccount(str(p.id, "id"), !!p.removeFiles));
  },

  async updateServer(p) {
    const params = { account: str(p.account, "account") };
    if (p.incoming) params.incoming = toBackendServer(p.incoming, "incoming", false);
    if (p.outgoing) params.outgoing = toBackendServer(p.outgoing, "outgoing", false);
    if (p.password !== undefined) {
      if (typeof p.password !== "string") throw invalid("password must be a string");
      params.password = p.password;
    }
    return fromBackendAccount(await adminCall(() => admin().updateServer(params)));
  },

  async createIdentity(p) {
    return adminCall(() => admin().createIdentity({ ...identityFields(p), account: str(p.account, "account"), email: str(p.email, "email") }));
  },

  async updateIdentity(p) {
    return adminCall(() => admin().updateIdentity({ ...identityFields(p), id: str(p.id, "id") }));
  },

  async deleteIdentity(p) {
    await adminCall(() => admin().deleteIdentity(str(p.id, "id")));
  },

  async setDefaultIdentity(p) {
    await adminCall(() => admin().setDefaultIdentity(str(p.account, "account"), str(p.identity, "identity")));
  },
};

const FLAGS = {
  read: { read: true }, unread: { read: false },
  flagged: { flagged: true }, unflagged: { flagged: false },
  junk: { junk: true }, "not-junk": { junk: false },
};

// ─── Events ─────────────────────────────────────────────────────────

const changedTimers = new Map(); // folder id → { pending }

/** Emit `changed` for a folder now, then at most once more per CHANGED_DEBOUNCE_MS window. */
function changed(folder) {
  const key = folder || "";
  const t = changedTimers.get(key);
  if (t) { t.pending = true; return; }
  emit("changed", { folder: key });
  const state = { pending: false };
  changedTimers.set(key, state);
  const tick = () => {
    if (!state.pending) { changedTimers.delete(key); return; }
    state.pending = false;
    emit("changed", { folder: key });
    setTimeout(tick, CHANGED_DEBOUNCE_MS);
  };
  setTimeout(tick, CHANGED_DEBOUNCE_MS);
}

function changedFor(list) {
  const folders = new Set((list?.messages || []).map((m) => (m.folder ? folderId(m.folder) : "")));
  if (folders.size === 0) folders.add("");
  folders.forEach(changed);
}

function listen() {
  api.messages.onNewMailReceived.addListener(async (folder, list) => {
    try {
      const { messages } = await collect(Promise.resolve(list), Infinity);
      emit("newMail", { folder: folderId(folder), messages });
    } catch (e) {
      console.error("[lazythunder] newMail:", e);
    }
  });
  api.messages.onUpdated.addListener((msg) => changed(msg?.folder ? folderId(msg.folder) : ""));
  api.messages.onMoved.addListener((from, to) => { changedFor(from); changedFor(to); });
  api.messages.onDeleted.addListener((list) => changedFor(list));
  api.folders.onFolderInfoChanged.addListener((folder) => changed(folder ? folderId(folder) : ""));
  for (const ns of [api.accounts, api.identities]) {
    for (const ev of ["onCreated", "onDeleted", "onUpdated"]) ns?.[ev]?.addListener(() => accountsChanged());
  }
}

let accountsTimer = null;
/** Emit accountsChanged at most once per CHANGED_DEBOUNCE_MS, trailing. */
function accountsChanged() {
  if (accountsTimer) return;
  accountsTimer = setTimeout(() => { accountsTimer = null; emit("accountsChanged", {}); }, CHANGED_DEBOUNCE_MS);
}

// ─── Account mapping ────────────────────────────────────────────────

// Thunderbird's nsMsgSocketType / nsMsgAuthMethod values.
// Every value has a name, so a server's settings survive a read-edit-write unchanged.
const SOCKET = { plain: 0, "starttls-optional": 1, starttls: 2, tls: 3 };
const SOCKET_NAMES = Object.fromEntries(Object.entries(SOCKET).map(([k, v]) => [v, k]));
const AUTH = {
  default: 0, none: 1, old: 2, "password-cleartext": 3, "password-encrypted": 4, gssapi: 5, ntlm: 6, external: 7,
  secure: 8, any: 9, oauth2: 10,
};
const AUTH_NAMES = Object.fromEntries(Object.entries(AUTH).map(([k, v]) => [v, k]));
const SERVER_TYPES = { incoming: ["imap", "pop3"], outgoing: ["smtp"] };

function admin() {
  if (!api.accountsAdmin) throw codeErr("THUNDERBIRD_ERROR", "account management API is not available");
  return api.accountsAdmin;
}

/** Runs an experiment call, turning its "CODE: message" errors into coded errors. */
async function adminCall(fn) {
  try {
    return await fn();
  } catch (e) {
    const m = /^([A-Z_]+): ([\s\S]*)$/.exec(e?.message || "");
    if (m) throw codeErr(m[1], m[2]);
    throw e;
  }
}

/** Protocol Server → backend fields. full requires every field (new account); else only given ones. */
function toBackendServer(s, side, full) {
  if (!s || typeof s !== "object") throw invalid(`${side} must be an object`);
  const out = {};
  if (s.type !== undefined || full) {
    if (!SERVER_TYPES[side].includes(s.type)) throw invalid(`${side}.type must be one of ${SERVER_TYPES[side].join(", ")}`);
    out.type = s.type;
  }
  if (s.hostname !== undefined || full) out.hostname = str(s.hostname, `${side}.hostname`);
  if (s.port !== undefined || full) {
    if (!Number.isInteger(s.port) || s.port < 1 || s.port > 65535) throw invalid(`${side}.port must be 1-65535`);
    out.port = s.port;
  }
  if (s.socketType !== undefined || full) {
    if (!Object.hasOwn(SOCKET, s.socketType)) throw invalid(`${side}.socketType must be one of ${Object.keys(SOCKET).join(", ")}`);
    out.socketType = SOCKET[s.socketType];
  }
  if (s.auth !== undefined || full) {
    if (!Object.hasOwn(AUTH, s.auth)) throw invalid(`${side}.auth must be one of ${Object.keys(AUTH).join(", ")}`);
    out.auth = AUTH[s.auth];
  }
  if (s.username !== undefined) {
    if (typeof s.username !== "string") throw invalid(`${side}.username must be a string`);
    out.username = s.username;
  }
  return out;
}

function fromBackendServer(s) {
  if (!s) return null;
  return {
    type: s.type, hostname: s.hostname, port: s.port,
    socketType: SOCKET_NAMES[s.socketType] ?? String(s.socketType), auth: AUTH_NAMES[s.auth] ?? String(s.auth),
    username: s.username || "",
  };
}

function fromBackendAccount(a) {
  return { ...a, incoming: fromBackendServer(a.incoming), outgoing: fromBackendServer(a.outgoing) };
}

function fromBackendConfig(c) {
  return { incoming: fromBackendServer(c.incoming), outgoing: fromBackendServer(c.outgoing), source: c.source };
}

function identityFields(p) {
  const out = {};
  for (const k of ["email", "name", "replyTo", "organization", "signature"]) {
    if (p[k] === undefined) continue;
    if (typeof p[k] !== "string") throw invalid(`${k} must be a string`);
    out[k] = p[k];
  }
  if (p.signatureIsHTML !== undefined) out.signatureIsHTML = !!p.signatureIsHTML;
  return out;
}

// ─── Helpers ────────────────────────────────────────────────────────

function folderId(f) {
  return f.id || `${f.accountId}:/${f.path}`;
}

function formatMessage(msg) {
  return {
    id: msg.id,
    date: msg.date && !isNaN(new Date(msg.date)) ? new Date(msg.date).toISOString() : null,
    author: msg.author || "",
    subject: msg.subject || "",
    read: !!msg.read,
    flagged: !!msg.flagged,
    junk: !!msg.junk,
    tags: msg.tags || [],
    folder: msg.folder ? { accountId: msg.folder.accountId, path: msg.folder.path, name: msg.folder.name } : null,
    recipients: msg.recipients || [],
    ccList: msg.ccList || [],
    headerMessageId: msg.headerMessageId || "",
  };
}

function extractParts(part, result = { text: "", html: "", attachments: [] }) {
  if (!part) return result;
  const ct = (part.contentType || "").toLowerCase();
  // A named or Content-Disposition: attachment part is an attachment even if it is text/*.
  const isAttachment = part.name ||
    part.headers?.["content-disposition"]?.some((v) => /^attachment\b/i.test(v));
  const att = () => ({ name: part.name || "unnamed", contentType: ct, partName: part.partName, size: part.size || 0 });
  if (isAttachment && part.partName) {
    result.attachments.push(att());
    return result;
  }
  if (ct === "text/plain" && part.body) result.text += part.body;
  else if (ct === "text/html" && part.body) result.html += part.body;
  else if (ct && !ct.startsWith("multipart/") && part.partName && ct !== "text/plain" && ct !== "text/html") {
    result.attachments.push(att());
  }
  for (const sub of part.parts || []) extractParts(sub, result);
  return result;
}

/** Walk a paged MessageList; stop at limit+1 matches (proving hasMore) and abort the list. */
async function collect(firstPage, limit) {
  let page = await firstPage;
  const messages = [];
  try {
    while (page) {
      for (const m of page.messages || []) {
        if (messages.length >= limit) return { messages, hasMore: true };
        messages.push(formatMessage(m));
      }
      if (!page.id) break;
      page = await api.messages.continueList(page.id);
    }
    page = null;
    return { messages, hasMore: false };
  } finally {
    if (page?.id) await api.messages.abortList(page.id).catch(() => {});
  }
}

/** Header block of a raw message, without the separating blank line. */
function headerBlock(raw) {
  const end = raw.search(/\r?\n\r?\n/);
  return end === -1 ? raw : raw.slice(0, end);
}

/** Header block of getRaw's result (MV2: binary string; else File), decoded as UTF-8 for 8-bit headers. */
async function rawHeaders(raw) {
  if (typeof raw !== "string") return headerBlock(new TextDecoder().decode(await raw.arrayBuffer()));
  return new TextDecoder().decode(Uint8Array.from(headerBlock(raw), (c) => c.charCodeAt(0)));
}

function parseHeader(raw, name) {
  const unfolded = headerBlock(raw).replace(/\r?\n[ \t]+/g, " ");
  const m = unfolded.match(new RegExp(`^${name}:[ \\t]*(.+)`, "im"));
  return m ? m[1].trim() : "";
}

function stripAngle(s) {
  return s.trim().replace(/^<|>$/g, "").trim();
}

function parseReferences(value) {
  return (value.match(/<[^>]+>/g) || []).map(stripAngle);
}

/** "Re: WG: [list] Call for Participants" → "Call for Participants" */
function normalizeSubject(subject) {
  return subject.trim().replace(/^((Re|WG|AW|Fwd?|FW|Sv|Vs|Ref):\s*|\[[^\]]*\]\s*)*/gi, "").trim();
}

function buildThreadIds(raw) {
  return new Set([
    ...parseReferences(parseHeader(raw, "References")),
    ...parseReferences(parseHeader(raw, "In-Reply-To")),
    stripAngle(parseHeader(raw, "Message-ID")),
  ].filter(Boolean));
}

async function prependReply(tabId, text) {
  const original = await api.compose.getComposeDetails(tabId);
  if (original.isPlainText) {
    await api.compose.setComposeDetails(tabId, { plainTextBody: text + "\n\n" + (original.plainTextBody || "") });
    return;
  }
  const doc = new DOMParser().parseFromString(original.body || "", "text/html");
  const intro = doc.createElement("div");
  for (const para of text.trim().split(/\n\s*\n/)) {
    const p = doc.createElement("p");
    p.textContent = para.replace(/\s*\n\s*/g, " ");
    intro.append(p);
  }
  doc.body.prepend(intro);
  await api.compose.setComposeDetails(tabId, { body: doc.documentElement.outerHTML });
}

async function closeTab(tabId) {
  try { await api.tabs.remove(tabId); } catch { /* already closed (e.g. after sendNow) */ }
}

/** Identity id for a sender address (case-insensitive); an identity id is accepted as is. */
async function resolveIdentity(value) {
  const all = (await api.accounts.list(false)).flatMap((a) => a.identities || []);
  if (!value.includes("@")) {
    if (all.some((i) => i.id === value)) return value;
    throw codeErr("NOT_FOUND", `Identity not found: ${value}`);
  }
  const want = value.trim().toLowerCase();
  const hits = all.filter((i) => (i.email || "").toLowerCase() === want);
  if (hits.length === 0) throw codeErr("NOT_FOUND", `No sender identity with address ${value}`);
  return hits[0].id;
}

function prune(o) {
  return Object.fromEntries(Object.entries(o).filter(([, v]) => v !== undefined));
}

function bytesToBase64(bytes) {
  let s = "";
  for (let i = 0; i < bytes.length; i += BASE64_CHUNK) s += String.fromCharCode(...bytes.subarray(i, i + BASE64_CHUNK));
  return btoa(s);
}

function codeErr(code, message) {
  return Object.assign(new Error(message), { code });
}

function invalid(message) {
  return codeErr("INVALID_ARGS", message);
}

function str(v, name) {
  if (typeof v !== "string" || v === "") throw invalid(`${name} must be a non-empty string`);
  return v;
}

function msgId(v) {
  if (!Number.isSafeInteger(v) || v <= 0) throw invalid("message id must be a positive integer");
  return v;
}

function msgIds(v) {
  if (!Array.isArray(v) || v.length === 0) throw invalid("ids must be a non-empty array");
  return v.map(msgId);
}

function date(v, name) {
  const d = new Date(str(v, name));
  if (isNaN(d)) throw invalid(`${name} must be an RFC 3339 date`);
  return d;
}

// Global cap on concurrent messenger.* calls so bulk work can't flood Thunderbird.
let inFlight = 0;
const queue = [];
function mapLimited(items, fn) {
  return Promise.all(items.map((item) => new Promise((resolve, reject) => {
    queue.push({ run: () => fn(item), resolve, reject });
    drain();
  })));
}
function drain() {
  while (inFlight < IPC_CONCURRENCY && queue.length) {
    const { run, resolve, reject } = queue.shift();
    inFlight++;
    Promise.resolve().then(run).then(resolve, reject).finally(() => { inFlight--; drain(); });
  }
}

if (typeof module !== "undefined") {
  module.exports = { handle, changed, toBackendServer, fromBackendServer, formatMessage, extractParts, headerBlock, buildThreadIds, normalizeSubject, setPort: (p) => { port = p; } };
} else {
  listen();
  connect();
}
