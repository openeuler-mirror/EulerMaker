import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { compile } from "@vue/compiler-dom";
import { parse } from "@vue/compiler-sfc";
import { renderToString } from "@vue/server-renderer";
import * as Vue from "vue";
import ts from "typescript";

const read = path => readFileSync(new URL(path, import.meta.url), "utf8");
const source = read("../src/components/PersonalCredentials.vue");
const { descriptor } = parse(source);
const transpile = code => ts.transpileModule(code, {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext },
}).outputText;
const script = transpile(descriptor.scriptSetup.content.replace(/^import .*;\n/gm, ""));
const render = new Function("Vue", transpile(compile(descriptor.template.content, {
  mode: "function", prefixIdentifiers: true, expressionPlugins: ["typescript"],
}).code))(Vue);
const api = new Function("sessionStorage", transpile(read("../src/api.ts").replace(/^import .*;\n/gm, "").replace(/^export /gm, "")) + "\nreturn { ApiError, request };")({ getItem: () => "browser-jwt" });
const credential = (id = "abc") => ({ id, name: "Workstation", createdAt: "2026-01-01T00:00:00Z", expiresAt: "2099-01-01T00:00:00Z" });
const defer = () => { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; };

function setup() {
  const calls = [], redirects = [], unmount = [];
  const transport = { run: async () => ({ items: [] }) };
  const session = Vue.reactive({ username: "alice", authenticated: true, role: "user", signOut() { this.authenticated = false; } });
  const deps = {
    ref: Vue.ref, computed: Vue.computed, watch: Vue.watch,
    window: { addEventListener() {}, removeEventListener() {} },
    onMounted: () => {}, onBeforeUnmount: fn => unmount.push(fn),
    useI18n: () => ({ t: (key, args) => key + (args?.name ? `:${args.name}` : ""), locale: Vue.ref("en-US") }),
    useSessionStore: () => session, useRouter: () => ({ replace: target => redirects.push(target) }),
    ApiError: api.ApiError, errorTranslationKey: (error, fallback) => error instanceof api.ApiError ? error.translationKey : fallback,
    request: async (path, options) => { calls.push({ path, ...options }); return transport.run(path, options); },
  };
  const scope = Vue.effectScope();
  const state = scope.run(() => new Function(...Object.keys(deps), script + `
    return { t, session, items, activeItems, loaded, loading, busy, canCreate, loadError, dialogError, notice,
      createOpen, name, days, uncertain, secret, copyMessage, revoking, load, openCreate, closeCreate,
      create, closeSecret, copySecret, openRevoke, closeRevoke, revoke, formatDate, clear };
  `)(...Object.values(deps)));
  const html = async () => {
    const app = Vue.createSSRApp({ setup: () => state, render });
    app.component("ModalDialog", { props: ["titleId", "title", "closeLabel"], emits: ["close"], setup: (_, { slots }) => () => slots.default?.() });
    app.component("Refresh", { render: () => null });
    return renderToString(app);
  };
  return { state, calls, transport, session, redirects, html, dispose: () => { unmount.forEach(fn => fn()); scope.stop(); } };
}

test("settings includes credentials independently of password login; both languages cover all text", () => {
  const settings = read("../src/views/SettingsView.vue");
  assert.match(settings, /<PersonalCredentials v-if="session.authenticated"/);
  assert.match(read("../src/router.ts"), /path: "\/settings"[^\n]+authenticated: true/);
  const keys = new Set([...source.matchAll(/"credentials\.([a-zA-Z]+)"|'credentials\.([a-zA-Z]+)'/g)].map(match => match[1] || match[2]));
  for (const locale of ["zh-CN", "en-US"]) {
    const messages = read(`../src/i18n/messages/${locale}.ts`);
    const entries = new Function(`return ({${messages.match(/  credentials: \{([\s\S]*?)\n  \},/)[1]}});`)();
    for (const key of keys) assert.ok(entries[key], `${locale}: ${key}`);
  }
  assert.doesNotMatch(source, /(?:localStorage|sessionStorage|saveToken|console\.)/);
});

test("list contains only active metadata, displays quota, and fails visibly on 403/503", async t => {
  const f = setup(); t.after(f.dispose);
  f.transport.run = async () => ({ items: [
    { ...credential(), token: "NEVER-RENDER", hash: "PRIVATE-HASH" },
    { ...credential("expired"), expiresAt: "2000-01-01T00:00:00Z" },
  ] });
  await f.state.load();
  assert.deepEqual(f.state.items.value[0], credential());
  assert.equal(f.state.activeItems.value.length, 1);
  const html = await f.html();
  assert.match(html, /1\/5/);
  assert.doesNotMatch(html, /NEVER-RENDER|PRIVATE-HASH/);
  assert.equal(f.calls[0].path, "/auth/credentials");
  assert.equal(f.calls[0].cache, "no-store");
  assert.equal(f.calls[0].credentials, "omit");
  for (const status of [403, 503]) {
    f.transport.run = async () => { throw new api.ApiError(status, `errors.${status}`); };
    await f.state.load();
    assert.equal(f.state.canCreate.value, false);
    assert.match(await f.html(), new RegExp(`errors.${status}`));
    assert.doesNotMatch(await f.html(), /credentials.empty/);
  }
});

test("creation validates input, defaults to 90 days, prevents double submit and reveals only once", async t => {
  const f = setup(); t.after(f.dispose);
  await f.state.load();
  f.state.openCreate();
  assert.equal(f.state.days.value, 90);
  for (const [name, days] of [["", 90], ["x".repeat(65), 90], ["ok", 0], ["ok", 91], ["ok", 1.5]]) {
    f.state.name.value = name; f.state.days.value = days;
    await f.state.create();
    assert.equal(f.calls.length, 1);
    assert.equal(f.state.dialogError.value, "credentials.invalidInput");
  }
  f.state.name.value = "  laptop  "; f.state.days.value = 90;
  const pending = defer();
  f.transport.run = async (_, options) => options.method === "POST" ? pending.promise : { items: [credential()] };
  const creation = f.state.create();
  await f.state.create();
  f.state.closeCreate();
  assert.equal(f.state.createOpen.value, true);
  assert.deepEqual(JSON.parse(f.calls[1].body), { name: "laptop", expiresInDays: 90 });
  assert.equal(f.calls.filter(call => call.method === "POST").length, 1);
  pending.resolve({ ...credential(), token: "ONCE-ONLY" });
  await creation;
  assert.match(await f.html(), /ONCE-ONLY/);
  assert.equal(f.state.createOpen.value, false);
  f.state.closeSecret();
  assert.doesNotMatch(await f.html(), /ONCE-ONLY/);
  assert.equal(f.state.secret.value, "");
});

test("local limit and concurrent quota rejection refresh the list without losing the draft", async t => {
  const f = setup(); t.after(f.dispose);
  await f.state.load(); f.state.openCreate();
  f.state.name.value = "draft"; f.state.days.value = 7;
  f.transport.run = async (_, options) => {
    if (options.method === "POST") throw new api.ApiError(409, "errors.conflict", "CredentialLimitExceeded");
    return { items: Array.from({ length: 5 }, (_, i) => credential(String(i))) };
  };
  await f.state.create();
  assert.equal(f.state.dialogError.value, "credentials.limit");
  assert.equal(f.state.canCreate.value, false);
  assert.equal(f.state.name.value, "draft");
  assert.equal(f.state.days.value, 7);
  assert.match(await f.html(), /5\/5/);
  await f.state.create();
  assert.equal(f.calls.filter(call => call.method === "POST").length, 1);
});

test("unknown creation outcomes never retry and require list review before another creation", async t => {
  for (const failure of [new Error("offline SECRET"), new api.ApiError(503, "errors.unavailable"), new SyntaxError("malformed")]) {
    const f = setup(); t.after(f.dispose);
    await f.state.load(); f.state.openCreate(); f.state.name.value = "draft";
    f.transport.run = async (_, options) => { if (options.method === "POST") throw failure; return { items: [credential()] }; };
    await f.state.create(); await f.state.create();
    assert.equal(f.state.uncertain.value, true);
    assert.equal(f.state.canCreate.value, false);
    assert.equal(f.state.secret.value, "");
    assert.equal(f.state.name.value, "draft");
    assert.equal(f.calls.filter(call => call.method === "POST").length, 1);
    assert.doesNotMatch(await f.html(), /offline SECRET/);
    f.state.closeCreate(); f.state.uncertain.value = false;
    assert.equal(f.state.canCreate.value, true);
  }
});

test("revoke requires confirmation, prevents duplicate calls and refreshes after success", async t => {
  const f = setup(); t.after(f.dispose);
  await f.state.load(); f.state.openRevoke(credential("id/encoded"));
  assert.equal(f.calls.length, 1);
  assert.match(await f.html(), /credentials.revokeConfirm:Workstation/);
  f.state.closeRevoke(); await f.state.revoke(); assert.equal(f.calls.length, 1);
  f.state.openRevoke(credential("id/encoded"));
  const pending = defer();
  f.transport.run = async (_, options) => options.method === "DELETE" ? pending.promise : { items: [] };
  const revocation = f.state.revoke(); await f.state.revoke();
  assert.equal(f.calls[1].path, "/auth/credentials/id%2Fencoded");
  assert.equal(f.calls[1].body, undefined);
  pending.resolve(); await revocation;
  assert.equal(f.state.revoking.value, null);
  assert.equal(f.state.notice.value, "credentials.revoked");
  assert.equal(f.calls.length, 3);
});

test("401 clears the session and routes back to login", async t => {
  const f = setup(); t.after(f.dispose);
  f.state.secret.value = "SECRET";
  f.transport.run = async () => { throw new api.ApiError(401, "errors.unauthorized"); };
  await f.state.load();
  assert.equal(f.session.authenticated, false);
  assert.equal(f.state.secret.value, "");
  assert.deepEqual(f.redirects, [{ name: "login", query: { redirect: "/settings" } }]);
});

test("privileged users see a permission warning and revoke failures remain actionable", async t => {
  const f = setup(); t.after(f.dispose);
  await f.state.load();
  for (const role of ["ops", "admin"]) {
    f.session.role = role;
    assert.match(await f.html(), /credentials.privilegedHint/);
  }
  f.session.role = "user";
  assert.doesNotMatch(await f.html(), /credentials.privilegedHint/);
  f.state.openRevoke(credential());
  f.transport.run = async () => { throw new Error("network failure"); };
  await f.state.revoke();
  assert.equal(f.state.revoking.value.id, "abc");
  assert.equal(f.state.busy.value, false);
  assert.equal(f.state.dialogError.value, "credentials.revokeFailed");
  assert.equal(f.calls.filter(call => call.method === "DELETE").length, 1);
});

test("switching users clears drafts and rejects a stale list response", async t => {
  const f = setup(); t.after(f.dispose);
  const pending = defer();
  f.transport.run = () => pending.promise;
  const oldLoad = f.state.load();
  f.state.name.value = "old draft";
  f.state.secret.value = "OLD-SECRET";
  f.transport.run = async () => ({ items: [credential("bob-credential")] });
  f.session.username = "bob";
  assert.equal(f.state.secret.value, "");
  assert.equal(f.state.name.value, "");
  pending.resolve({ items: [credential("alice-credential")] });
  await oldLoad;
  assert.deepEqual(f.state.items.value.map(item => item.id), ["bob-credential"]);
});

test("logout and unmount clear secrets and ignore late creation responses", async t => {
  for (const leave of [f => f.session.signOut(), f => f.dispose()]) {
    const f = setup(); t.after(f.dispose);
    await f.state.load(); f.state.openCreate(); f.state.name.value = "draft";
    const pending = defer(); f.transport.run = () => pending.promise;
    const creation = f.state.create();
    leave(f);
    assert.equal(f.calls[1].signal.aborted, true);
    pending.resolve({ ...credential(), token: "LATE-SECRET" });
    await creation;
    assert.equal(f.state.secret.value, "");
    assert.equal(f.state.name.value, "");
    assert.equal(f.state.loaded.value, false);
  }
});

test("copy failure keeps the secret available for manual copy and closing clears it", async t => {
  const original = Object.getOwnPropertyDescriptor(globalThis, "navigator");
  t.after(() => original ? Object.defineProperty(globalThis, "navigator", original) : delete globalThis.navigator);
  Object.defineProperty(globalThis, "navigator", { configurable: true, value: { clipboard: { writeText: async () => { throw new Error("denied"); } } } });
  const f = setup(); t.after(f.dispose);
  f.state.secret.value = "COPY-MANUALLY";
  await f.state.copySecret();
  assert.equal(f.state.copyMessage.value, "credentials.copyFailed");
  assert.match(await f.html(), /readonly[^>]*>COPY-MANUALLY/);
  f.state.closeSecret();
  assert.equal(f.state.secret.value, "");
  assert.equal(f.state.copyMessage.value, "");
  const copied = [];
  navigator.clipboard.writeText = async value => { copied.push(value); };
  f.state.secret.value = "COPY-ONCE";
  await f.state.copySecret();
  assert.deepEqual(copied, ["COPY-ONCE"]);
  assert.equal(f.state.copyMessage.value, "credentials.copied");
  const pending = defer();
  navigator.clipboard.writeText = () => pending.promise;
  const copying = f.state.copySecret();
  f.state.closeSecret();
  pending.resolve(); await copying;
  assert.equal(f.state.copyMessage.value, "");
});

test("shared request preserves safe error codes without raw response bodies or retry", async t => {
  const original = globalThis.fetch; t.after(() => { globalThis.fetch = original; });
  let calls = 0;
  globalThis.fetch = async (path, options) => {
    calls++;
    assert.equal(path, "/auth/credentials");
    assert.equal(options.headers.get("Authorization"), "Bearer browser-jwt");
    return new Response(JSON.stringify({ code: "CredentialConflict", error: "PRIVATE" }), { status: 409 });
  };
  await assert.rejects(api.request("/auth/credentials", { method: "POST" }), error => {
    assert.equal(error.code, "CredentialConflict");
    assert.equal(error.translationKey, "errors.conflict");
    assert.doesNotMatch(error.message, /PRIVATE/);
    return true;
  });
  assert.equal(calls, 1);
});
