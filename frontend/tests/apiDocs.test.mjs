import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const source = readFileSync(new URL("../src/utils/apiDocs.ts", import.meta.url), "utf8");
const js = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } }).outputText;
const { loadApiDocument, readOnlySwaggerOptions } = await import(`data:text/javascript;base64,${Buffer.from(js).toString("base64")}`);

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
  assert.equal(options.deepLinking, true);
  assert.equal(options.plugins[0]().components.authorizeBtn(), null);
  assert.equal(options.plugins[0]().components.authorizeOperationBtn(), null);
  assert.throws(options.requestInterceptor, /read-only/);
});

test("API documentation has public navigation and an exact proxy outside SPA fallback", () => {
  const read = (file) => readFileSync(new URL(file, import.meta.url), "utf8");
  assert.match(read("../src/router.ts"), /path: "\/api-docs", name: "api-docs", component:/);
  assert.match(read("../src/App.vue"), /<RouterLink to="\/api-docs">/);
  assert.match(read("../deploy/nginx.conf"), /location = \/openapi\.json\s*\{\s*proxy_pass http:\/\/ebs-gateway:8080;/);
  assert.match(read("../deploy/nginx.conf"), /map \$uri \$frame_ancestors\s*\{\s*default "'none'";\s*\/api-docs-viewer\.html "'self'";/);
  assert.match(read("../vite.config.ts"), /openapi\\\\\.json/);
  assert.match(read("../src/views/ApiDocsView.vue"), /\/api-docs-viewer\.html/);
});
