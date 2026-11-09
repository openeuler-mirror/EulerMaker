import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { parse } from "@vue/compiler-sfc";
import ts from "typescript";

function compile(source) {
  return ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } }).outputText;
}
const source = readFileSync(new URL("../src/oauth.ts", import.meta.url), "utf8");
const oauth = await import("data:text/javascript;base64," + Buffer.from(compile(source)).toString("base64"));
function storage() {
  const data = new Map();
  globalThis.sessionStorage = {
    getItem: key => data.get(key) ?? null,
    setItem: (key, value) => data.set(key, value),
    removeItem: key => data.delete(key),
  };
  return data;
}
const context = () => ({ registrationRequired: true, registrationID: "registration-1",
  expiresAt: new Date(Date.now() + 600000).toISOString(), profile: { displayName: "Alice", email: "alice@example.com" } });

test("login modes gate local forms and callback capture precedes router initialization", () => {
  const read = path => readFileSync(new URL(path, import.meta.url), "utf8");
  const login = read("../src/views/LoginView.vue");
  const register = read("../src/views/RegisterView.vue");
  assert.match(login, /t\("oauth.communityLogin"\)/);
  assert.match(login, /v-else-if="!loginMethods.methods.passwordLoginEnabled"/);
  assert.match(login, /if \(!loginMethods.methods\?\.passwordLoginEnabled/);
  assert.match(register, /v-else-if="!loginMethods.methods.registrationEnabled"/);
  assert.match(register, /if \(!loginMethods.methods\?\.registrationEnabled/);
  assert.match(read("../src/App.vue"), /v-if="loginMethods.methods\?\.registrationEnabled"/);
  const main = read("../src/main.ts");
  assert.ok(main.indexOf("captureOAuthCallback();") < main.indexOf('await import("@/router")'));
});

test("callback is captured once and URL is cleared before use", () => {
  storage();
  let cleanURL;
  globalThis.window = {
    location: { pathname: "/auth/callback", search: "?code=private-code&state=community.random&iss=https%3A%2F%2Fid.example" },
    history: { state: { position: 0 }, replaceState: (_state, _title, url) => { cleanURL = url; } },
  };
  oauth.captureOAuthCallback();
  assert.equal(cleanURL, "/auth/callback");
  assert.deepEqual(oauth.takeOAuthCallback(), { provider: "community", body: {
    code: "private-code", state: "community.random", iss: "https://id.example",
  } });
  assert.throws(() => oauth.takeOAuthCallback(), /ContextExpired/);
});

test("callback supports cancellation and rejects ambiguous credential parameters", () => {
  assert.deepEqual(oauth.parseCallback("?state=community.random&error=access_denied&error_description=private"), {
    provider: "community", body: { state: "community.random", error: "access_denied" },
  });
  for (const search of ["?code=x&code=y&state=community.x", "?code=x&state=community.x&state=community.y",
    "?code=x&error=denied&state=community.x", "?code=x&state=../x", "?code=x&state=community."]) {
    assert.throws(() => oauth.parseCallback(search), /InvalidRequest/);
  }
});

test("provider navigation uses fixed same-origin routes and safe business destinations", () => {
  const provider = { id: "community", loginURL: "/auth/oauth/community/login" };
  assert.equal(oauth.providerLoginURL(provider, "/projects/demo?tab=builds"), "/auth/oauth/community/login?returnTo=%2Fprojects%2Fdemo%3Ftab%3Dbuilds");
  for (const path of ["https://evil.example", "//evil", "/\\evil", "/auth/callback", "/projects/../login", "/%2f/evil"]) {
    assert.equal(oauth.safeReturnTo(path), "/projects");
  }
  assert.throws(() => oauth.providerLoginURL({ ...provider, loginURL: "https://evil.example" }, "/"));
});

test("registration context and submitted draft survive refresh without storing credentials", () => {
  const saved = storage();
  oauth.beginRegistration(context());
  const draft = oauth.readRegistration();
  assert.equal(draft.displayName, "Alice");
  assert.equal(draft.email, "alice@example.com");
  assert.equal(draft.submitted, false);
  oauth.saveRegistration({ ...draft, username: "alice", submitted: true });
  assert.equal(oauth.readRegistration().submitted, true);
  assert.equal(oauth.readRegistration().username, "alice");
  assert.equal(saved.size, 1);
  oauth.saveRegistration({ ...draft, expiresAt: "2000-01-01T00:00:00Z" });
  assert.equal(oauth.readRegistration(), null);
  assert.equal(saved.size, 0);
});

test("OAuth requests send cookies but no existing bearer, and preserve safe error codes", async t => {
  storage();
  const calls = [];
  t.mock.method(globalThis, "fetch", async (path, init) => {
    calls.push({ path, init });
    return new Response(JSON.stringify({ code: "RegistrationInProgress", error: "private upstream diagnostic" }), { status: 409 });
  });
  await assert.rejects(oauth.oauthRequest("/auth/oauth/register", { action: "confirm" }), error => {
    assert.equal(error.code, "RegistrationInProgress");
    assert.equal(error.status, 409);
    assert.equal(error.message, "RegistrationInProgress");
    return true;
  });
  assert.equal(calls[0].init.credentials, "same-origin");
  assert.equal(calls[0].init.cache, "no-store");
  assert.equal(calls[0].init.headers.Authorization, undefined);
  assert.equal(calls.length, 1);
});

function registrationPage(request) {
  const { descriptor } = parse(readFileSync(new URL("../src/views/OAuthRegisterView.vue", import.meta.url), "utf8"));
  const script = ts.createSourceFile("page.ts", descriptor.scriptSetup.content, ts.ScriptTarget.Latest, true);
  const statements = script.statements.filter(item => !ts.isImportDeclaration(item)).map(item => item.getText(script)).join("\n");
  const timers = new Map();
  let nextTimer = 0;
  const hooks = {};
  const state = {
    ...oauth,
    ref: value => ({ value }), computed: fn => ({ get value() { return fn(); } }), watch: () => {},
    onMounted: fn => { hooks.mount = fn; }, onBeforeUnmount: fn => { hooks.unmount = fn; },
    useI18n: () => ({ t: key => key }), useRouter: () => ({ replace: async () => {} }),
    useSessionStore: () => ({ completeOAuth: async () => {} }),
    oauthRequest: request,
    setTimeout: (fn, ms) => { timers.set(++nextTimer, { fn, ms }); return nextTimer; },
    clearTimeout: id => { timers.delete(id); },
  };
  const code = compile(statements).replace(/export \{\};?/, "");
  const page = new Function(...Object.keys(state), code + "\nreturn { submit, confirm, draft, busy, restartRequired, errorKey };")(...Object.values(state));
  return { ...page, hooks, timers };
}

test("unknown create outcome locks the draft and only issues bounded confirmations", async () => {
  storage(); oauth.beginRegistration(context());
  const calls = [];
  const page = registrationPage(async (_path, body) => {
    calls.push(body);
    assert.equal(oauth.readRegistration().submitted, true);
    throw new oauth.OAuthError(body.action === "create" ? "IAMUnavailable" : "RegistrationInProgress", body.action === "create" ? 503 : 409);
  });
  page.draft.value.username = "alice";
  await page.submit();
  await page.submit();
  assert.equal(calls.length, 1);
  assert.equal(page.draft.value.submitted, true);
  for (let attempt = 0; attempt < 3; attempt++) {
    const [id, timer] = [...page.timers][0];
    page.timers.delete(id);
    assert.equal(timer.ms, 3000);
    await page.confirm(true);
  }
  assert.equal(page.timers.size, 0);
  assert.deepEqual(calls.slice(1), Array(3).fill({ registrationID: "registration-1", username: "alice", action: "confirm" }));
  const restored = registrationPage(async (_path, body) => { assert.equal(body.action, "confirm"); });
  assert.equal(restored.draft.value.username, "alice");
  assert.equal(restored.draft.value.submitted, true);
});

test("confirmed username conflict allows editing, failed persistence sends no create", async () => {
  storage(); oauth.beginRegistration(context());
  const page = registrationPage(async () => { throw new oauth.OAuthError("UsernameAlreadyExists", 409); });
  page.draft.value.username = "alice";
  await page.submit();
  assert.equal(page.draft.value.submitted, false);
  assert.equal(oauth.readRegistration().submitted, false);
  let calls = 0;
  const blocked = registrationPage(async () => { calls++; });
  blocked.draft.value.username = "alice";
  sessionStorage.setItem = () => { throw Error("quota exceeded"); };
  await blocked.submit();
  assert.equal(calls, 0);
  assert.equal(blocked.restartRequired.value, true);
});

test("session replacement validates the OAuth token before writing browser storage", async t => {
  storage();
  const apiSource = readFileSync(new URL("../src/api.ts", import.meta.url), "utf8");
  const api = await import("data:text/javascript;base64," + Buffer.from(compile(apiSource)).toString("base64"));
  api.saveToken("existing");
  t.mock.method(globalThis, "fetch", async (_path, init) => {
    assert.equal(init.headers.Authorization, "Bearer candidate");
    return new Response("{}", { status: 503 });
  });
  await assert.rejects(api.acceptOAuthToken("candidate"));
  assert.equal(api.readToken(), "existing");
  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ identity: { name: "alice", scopes: ["ebs:user"] } })));
  const session = await api.acceptOAuthToken("candidate");
  assert.equal(session.identity.name, "alice");
  assert.equal(api.readToken(), "candidate");
});
