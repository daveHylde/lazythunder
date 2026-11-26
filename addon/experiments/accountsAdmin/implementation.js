/* Privileged account management for lazythunder (see addon/PROTOCOL.md).
 * Reuses Thunderbird's own account-setup modules, so lookup, login check and
 * account creation behave like Thunderbird's account hub. Socket types and auth
 * methods cross this boundary as Thunderbird's numeric enums; background.js
 * maps them to protocol strings. Errors carry a code as "CODE: message". */
"use strict";

var { ExtensionCommon } = ChromeUtils.importESModule("resource://gre/modules/ExtensionCommon.sys.mjs");
var { ExtensionUtils } = ChromeUtils.importESModule("resource://gre/modules/ExtensionUtils.sys.mjs");
var { MailServices } = ChromeUtils.importESModule("resource:///modules/MailServices.sys.mjs");

const lazy = {};
ChromeUtils.defineESModuleGetters(lazy, {
  AccountConfig: "resource:///modules/accountcreation/AccountConfig.sys.mjs",
  ConfigVerifier: "resource:///modules/accountcreation/ConfigVerifier.sys.mjs",
  CreateInBackend: "resource:///modules/accountcreation/CreateInBackend.sys.mjs",
  FetchConfig: "resource:///modules/accountcreation/FetchConfig.sys.mjs",
  GuessConfig: "resource:///modules/accountcreation/GuessConfig.sys.mjs",
});

const fail = (code, msg) => new ExtensionUtils.ExtensionError(`${code}: ${msg}`);

// Experiment scripts run in a sandbox without AbortController; system modules have it.
const { AbortController } = Cu.getGlobalForObject(MailServices);
var { setTimeout, clearTimeout } = ChromeUtils.importESModule("resource://gre/modules/Timer.sys.mjs");
const FIND_TIMEOUT_MS = 45000;

/** Unknown errors would reach the add-on as "An unexpected error occurred": pass the message on. */
function wrap(fns) {
  return Object.fromEntries(Object.entries(fns).map(([name, fn]) => [name, async (...args) => {
    try {
      return await fn(...args);
    } catch (e) {
      if (e instanceof ExtensionUtils.ExtensionError) throw e;
      console.error(`[lazythunder] accountsAdmin.${name}`, e);
      throw fail("THUNDERBIRD_ERROR", e?.message || String(e));
    }
  }]));
}

function account(id) {
  const a = MailServices.accounts.getAccount(id);
  if (!a || !a.incomingServer) throw fail("NOT_FOUND", `no account ${id}`);
  return a;
}

function identity(id) {
  const i = MailServices.accounts.getIdentity(id);
  if (!i || !i.valid) throw fail("NOT_FOUND", `no identity ${id}`);
  return i;
}

function ownerOf(i) {
  const a = MailServices.accounts.accounts.find((x) => x.identities.some((y) => y.key == i.key));
  if (!a) throw fail("NOT_FOUND", `no account owns identity ${i.key}`);
  return a;
}

function outgoingOf(acct) {
  const id = acct.defaultIdentity;
  return (id && MailServices.outgoingServer.getServerByIdentity(id)) || null;
}

function incomingInfo(s) {
  return { type: s.type, hostname: s.hostname, port: s.port, socketType: s.socketType, auth: s.authMethod, username: s.username };
}

function outgoingInfo(s) {
  if (!s) return null;
  const smtp = s.type == "smtp" ? s.QueryInterface(Ci.nsISmtpServer) : null;
  return {
    type: s.type,
    hostname: smtp ? smtp.hostname : s.serverURI?.host || "",
    port: smtp ? smtp.port : s.serverURI?.port ?? -1,
    socketType: s.socketType, auth: s.authMethod, username: s.username || "",
  };
}

function identityInfo(i, acct) {
  return {
    id: i.key, accountId: acct.key, email: i.email || "", name: i.fullName || "",
    replyTo: i.replyTo || "", organization: i.organization || "",
    signature: i.htmlSigText || "", signatureIsHTML: !!i.htmlSigFormat,
    default: acct.defaultIdentity?.key == i.key,
  };
}

function details(acct) {
  const s = acct.incomingServer;
  return {
    id: acct.key, name: s.prettyName, type: s.type,
    incoming: incomingInfo(s), outgoing: outgoingInfo(outgoingOf(acct)),
    identities: acct.identities.map((i) => identityInfo(i, acct)),
  };
}

function configInfo(c) {
  const side = (x) => ({ type: x.type, hostname: x.hostname, port: x.port, socketType: x.socketType, auth: x.auth, username: x.username || "" });
  return { incoming: side(c.incoming), outgoing: side(c.outgoing), source: c.source == "xml" ? "isp" : c.source || "guess" };
}

function hasWindow() {
  return !!Services.wm.getMostRecentWindow("mail:3pane");
}

function msgWindow() {
  return Services.wm.getMostRecentWindow("mail:3pane")?.msgWindow
    || Cc["@mozilla.org/messenger/msgwindow;1"].createInstance(Ci.nsIMsgWindow);
}

async function setPassword(server, password) {
  try { server.forgetPassword(); } catch {} // drops the stored login
  server.password = password;
  if (password) await lazy.CreateInBackend.rememberPassword(server, password);
}

function applyIdentity(i, p) {
  if (p.email !== undefined) i.email = p.email;
  if (p.name !== undefined) i.fullName = p.name;
  if (p.replyTo !== undefined) i.replyTo = p.replyTo;
  if (p.organization !== undefined) i.organization = p.organization;
  if (p.signature !== undefined) i.htmlSigText = p.signature;
  if (p.signatureIsHTML !== undefined) i.htmlSigFormat = !!p.signatureIsHTML;
}

this.accountsAdmin = class extends ExtensionCommon.ExtensionAPI {
  getAPI() {
    return {
      accountsAdmin: wrap({
        async details() {
          return MailServices.accounts.accounts.filter((a) => a.incomingServer && a.incomingServer.type != "none").map(details);
        },

        // Thunderbird's autoconfig sources in its priority order (local file,
        // the provider's own config, Mozilla's ISPDB, MX lookup), then guessing.
        // Microsoft Exchange autodiscover is left out: lazythunder sets up
        // IMAP/POP3 only, and it can wait minutes or send credentials elsewhere.
        async findConfig(email) {
          const domain = email.split("@")[1];
          if (!domain) throw fail("INVALID_ARGS", "email must contain @");
          const abort = new AbortController();
          const timer = setTimeout(() => abort.abort(), FIND_TIMEOUT_MS);
          try {
            const F = lazy.FetchConfig;
            const lookups = [
              () => F.fromDisk(domain, abort.signal),
              () => F.fromISP(domain, email, abort.signal),
              () => F.fromDB(domain, abort.signal),
              () => F.forMX(domain, email, abort.signal),
            ].map((f) => Promise.resolve().then(f));
            let config = null;
            for (const r of await Promise.allSettled(lookups)) {
              if (r.status == "fulfilled" && r.value) {
                config = r.value;
                break;
              }
            }
            if (!config && !abort.signal.aborted) {
              try {
                config = await lazy.GuessConfig.guessConfig(domain, () => {}, lazy.AccountConfig.guessConfigFromEmail(email), "both", abort.signal);
              } catch (e) {
                config = null;
              }
            }
            if (!config) throw fail("NOT_FOUND", `no settings found for ${domain}`);
            lazy.AccountConfig.replaceVariables(config, "x", email, "");
            return configInfo(config);
          } finally {
            clearTimeout(timer);
            abort.abort(); // stop lookups still running
          }
        },

        async createAccount(p) {
          const oauth = Ci.nsMsgAuthMethod.OAuth2;
          if ((p.incoming.auth == oauth || p.outgoing.auth == oauth) && !hasWindow()) {
            throw fail("NEEDS_WINDOW", "the sign-in page needs the Thunderbird window");
          }
          const config = new lazy.AccountConfig();
          config.source = lazy.AccountConfig.kSourceUser;
          Object.assign(config.incoming, p.incoming);
          Object.assign(config.outgoing, p.outgoing);
          config.incoming.password = config.outgoing.password = p.password || "";
          config.rememberPassword = !!p.password;
          lazy.AccountConfig.replaceVariables(config, p.name, p.email, p.password || "");
          if (lazy.CreateInBackend.checkIncomingServerAlreadyExists(config)) {
            throw fail("ALREADY_EXISTS", `${config.incoming.username} on ${config.incoming.hostname} is already set up`);
          }
          const existing = lazy.CreateInBackend.checkOutgoingServerAlreadyExists(config);
          if (existing) {
            config.outgoing.addThisServer = false;
            config.outgoing.existingServerKey = existing.key;
          }
          const abort = new AbortController();
          const verifier = new lazy.ConfigVerifier(msgWindow(), abort.signal);
          try {
            const ok = await verifier.verifyConfig(config, false);
            config.incoming.auth = ok.incoming.auth;
            config.outgoing.auth = ok.outgoing.auth;
            config.incoming.username = ok.incoming.username;
            config.outgoing.username = ok.outgoing.username;
          } catch (e) {
            throw fail("LOGIN_FAILED", e?.message || String(e));
          } finally {
            verifier.cleanup();
          }
          const acct = await lazy.CreateInBackend.createAccountInBackend(config);
          try {
            acct.incomingServer.getNewMessages(acct.incomingServer.rootFolder, msgWindow(), null);
          } catch (e) {
            console.warn("[lazythunder] first fetch failed", e);
          }
          return details(acct);
        },

        async deleteAccount(id, removeFiles) {
          MailServices.accounts.removeAccount(account(id), !!removeFiles);
        },

        async updateServer(p) {
          const acct = account(p.account);
          const s = acct.incomingServer;
          const inc = p.incoming || {};
          if (inc.hostname) s.hostname = inc.hostname;
          if (inc.port) s.port = inc.port;
          if (inc.socketType !== undefined) s.socketType = inc.socketType;
          if (inc.auth !== undefined) s.authMethod = inc.auth;
          if (inc.username) s.username = inc.username;
          const out = outgoingOf(acct);
          const o = p.outgoing || {};
          if (out && Object.keys(o).length) {
            const smtp = out.type == "smtp" ? out.QueryInterface(Ci.nsISmtpServer) : null;
            if (smtp && o.hostname) smtp.hostname = o.hostname;
            if (smtp && o.port) smtp.port = o.port;
            if (o.socketType !== undefined) out.socketType = o.socketType;
            if (o.auth !== undefined) out.authMethod = o.auth;
            if (o.username !== undefined) out.username = o.username;
          }
          if (p.password !== undefined && p.password !== "") {
            await setPassword(s, p.password);
            if (out && out.authMethod != Ci.nsMsgAuthMethod.none && out.authMethod != Ci.nsMsgAuthMethod.OAuth2) {
              await setPassword(out, p.password);
            }
          }
          return details(acct);
        },

        async createIdentity(p) {
          const acct = account(p.account);
          const i = MailServices.accounts.createIdentity();
          applyIdentity(i, p);
          const def = acct.defaultIdentity;
          if (def) i.smtpServerKey = def.smtpServerKey; // send through the account's server
          i.valid = true;
          acct.addIdentity(i);
          return identityInfo(i, acct);
        },

        async updateIdentity(p) {
          const i = identity(p.id);
          applyIdentity(i, p);
          return identityInfo(i, ownerOf(i));
        },

        async deleteIdentity(id) {
          const i = identity(id);
          const owner = ownerOf(i);
          if (owner.identities.length <= 1) throw fail("INVALID_ARGS", "an account needs at least one identity");
          owner.removeIdentity(i);
        },

        async setDefaultIdentity(accountId, identityId) {
          const acct = account(accountId);
          const i = acct.identities.find((x) => x.key == identityId);
          if (!i) throw fail("NOT_FOUND", `identity ${identityId} is not in ${accountId}`);
          acct.defaultIdentity = i;
        },
      }),
    };
  }
};
