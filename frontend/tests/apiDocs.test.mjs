import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const source = readFileSync(new URL("../src/utils/apiDocs.ts", import.meta.url), "utf8");
const js = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } }).outputText;
const { apiOperations, operationDocument, operationHash, fitApiDocsFrame, loadApiDocument, readOnlySwaggerOptions } = await import(`data:text/javascript;base64,${Buffer.from(js).toString("base64")}`);

test("directory operations preserve shared parameters, schemas and security in single-endpoint views", () => {
  const parameters = [{ name: "name", in: "path", required: true, schema: { type: "string" } }];
  const spec = {
    openapi: "3.0.3", components: { schemas: { Item: { type: "object" } } }, security: [{ BearerAuth: [] }],
    paths: {
      "/items/{name}": {
        parameters, summary: "Item", servers: [{ url: "/" }],
        get: { operationId: "readItem", tags: ["Items"], summary: "Read item", responses: {} },
        put: { operationId: "updateItem", tags: ["Items"], responses: {} },
      },
      "/auth/login": { post: { operationId: "login", tags: ["Authentication"], responses: {} } },
    },
  };
  const operations = apiOperations(spec);
  assert.equal(operations.length, 3);
  assert.deepEqual(operations[0], { path: "/items/{name}", method: "get", id: "readItem", tag: "Items", summary: "Read item" });
  const single = operationDocument(spec, operations[0]);
  assert.deepEqual(Object.keys(single.paths), ["/items/{name}"]);
  assert.deepEqual(Object.keys(single.paths["/items/{name}"]), ["parameters", "summary", "servers", "get"]);
  assert.equal(single.paths["/items/{name}"].parameters, parameters);
  assert.equal(single.components, spec.components);
  assert.equal(single.security, spec.security);
  assert.equal(apiOperations(spec).length, 3);
  assert.equal(operationHash(operations[0]), "#/Items/readItem");
  assert.equal(operationHash({ tag: "A/B", id: "read item" }), "#/A%2FB/read%20item");
});

test("API viewer follows expanded and filtered content height and disconnects on cleanup", (t) => {
  let height = 320;
  let resize;
  let observed;
  let disconnected = false;
  const previous = Object.getOwnPropertyDescriptor(globalThis, "ResizeObserver");
  globalThis.ResizeObserver = class {
    constructor(callback) { resize = callback; }
    observe(element) { observed = element; }
    disconnect() { disconnected = true; }
  };
  t.after(() => {
    if (previous) Object.defineProperty(globalThis, "ResizeObserver", previous);
    else delete globalThis.ResizeObserver;
  });
  const body = { getBoundingClientRect: () => ({ height }) };
  const viewport = Object.assign(new EventTarget(), { innerHeight: 1000 });
  const properties = new Map();
  const frame = {
    contentDocument: { body, documentElement: { style: { setProperty: (name, value) => properties.set(name, value) } } },
    ownerDocument: { defaultView: viewport }, style: {},
  };
  const cleanup = fitApiDocsFrame(frame);
  assert.equal(properties.get("--api-viewport-height"), "1000px");
  viewport.innerHeight = 700;
  viewport.dispatchEvent(new Event("resize"));
  assert.equal(properties.get("--api-viewport-height"), "700px");
  assert.equal(observed, body);
  assert.equal(frame.style.height, "320px");
  height = 2400.5;
  resize();
  assert.equal(frame.style.height, "2401px");
  height = 180;
  resize();
  assert.equal(frame.style.height, "180px");
  cleanup();
  assert.equal(disconnected, true);
  viewport.innerHeight = 900;
  viewport.dispatchEvent(new Event("resize"));
  assert.equal(properties.get("--api-viewport-height"), "700px");
});

test("API document is loaded anonymously from the fixed same-origin endpoint", async (t) => {
  const spec = { openapi: "3.0.3", paths: {} };
  const signal = new AbortController().signal;
  t.mock.method(globalThis, "fetch", async (url, options) => {
    assert.equal(url, "/openapi.json");
    assert.equal(options.credentials, "omit");
    assert.equal(options.cache, "no-cache");
    assert.equal(options.signal, signal);
    assert.deepEqual(options.headers, { Accept: "application/json" });
    return Response.json(spec);
  });
  assert.deepEqual(await loadApiDocument(signal), spec);
});

test("failed document loads can be retried without redirecting to login", async (t) => {
  const responses = [new Response("unavailable", { status: 503 }), new Response("<html>SPA fallback</html>"), Response.json({ error: "not a document" }), Response.json({ openapi: "3.0.3", paths: {} })];
  t.mock.method(globalThis, "fetch", async () => responses.shift());
  const signal = new AbortController().signal;
  await assert.rejects(loadApiDocument(signal), /HTTP 503/);
  await assert.rejects(loadApiDocument(signal), /Expected JSON/);
  await assert.rejects(loadApiDocument(signal), /Invalid OpenAPI/);
  assert.equal((await loadApiDocument(signal)).openapi, "3.0.3");
});

test("Swagger displays security requirements without credential inputs or execution", () => {
  const spec = { openapi: "3.0.3", paths: {}, components: { securitySchemes: { BearerAuth: {} } } };
  const options = readOnlySwaggerOptions(spec, {});
  assert.equal(options.spec, spec);
  assert.deepEqual(options.supportedSubmitMethods, []);
  assert.equal(options.persistAuthorization, false);
  assert.equal(options.validatorUrl, null);
  assert.equal(options.queryConfigEnabled, false);
  assert.equal(options.deepLinking, false);
  assert.equal(options.filter, false);
  assert.equal(options.docExpansion, "full");
  assert.equal(options.plugins[0]().components.authorizeBtn(), null);
  assert.equal(options.plugins[0]().components.authorizeOperationBtn(), null);
  assert.throws(options.requestInterceptor, /read-only/);
});

test("API documentation has public navigation and an exact proxy outside SPA fallback", () => {
  const read = (file) => readFileSync(new URL(file, import.meta.url), "utf8");
  assert.match(read("../src/router.ts"), /path: "\/api-docs", name: "api-docs", component:/);
  assert.match(read("../src/App.vue"), /<nav class="footer-links"[^>]*>\s*<RouterLink to="\/api-docs">[^]*?<\/RouterLink>\s*<a[^>]*>\{\{ t\("app.sourceRepository"\) \}\}<\/a>/);
  assert.match(read("../deploy/nginx.conf"), /location = \/openapi\.json\s*\{\s*proxy_pass http:\/\/ebs-gateway:8080;/);
  assert.match(read("../deploy/nginx.conf"), /map \$uri \$frame_ancestors\s*\{\s*default "'none'";\s*\/api-docs-viewer\.html "'self'";/);
  assert.match(read("../vite.config.ts"), /openapi\\\\\.json/);
  assert.match(read("../src/views/ApiDocsView.vue"), /\/api-docs-viewer\.html/);
  assert.match(read("../api-docs-viewer.html"), /class="api-directory-header">\s*<label id="search-label"[\s\S]*?<a id="api-docs-download" href="\/openapi\.json" download="openapi\.json"/);
  assert.match(read("../src/styles/api-docs.css"), /\.swagger-ui \.wrapper\s*\{\s*max-width: none; margin: 0; padding: 0;/);
});
