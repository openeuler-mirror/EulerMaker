import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import http from "node:http";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { createServer } from "vite";

test("Vite serves OAuth pages and forwards authentication APIs with cookies and Origin", { timeout: 30000 }, async () => {
  const received = [];
  const gateway = http.createServer((req, res) => {
    received.push({ path: req.url, method: req.method, cookie: req.headers.cookie, origin: req.headers.origin });
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({ ok: true }));
  });
  await new Promise(resolve => gateway.listen(0, "127.0.0.1", resolve));
  let vite;
  try {
    vite = await createServer({
      root: fileURLToPath(new URL("../", import.meta.url)),
      configFile: fileURLToPath(new URL("../vite.config.ts", import.meta.url)),
      logLevel: "silent",
      server: { host: "127.0.0.1", port: 0, proxy: { "/auth": { target: `http://127.0.0.1:${gateway.address().port}` } } },
    });
    await vite.listen();
    const base = `http://127.0.0.1:${vite.httpServer.address().port}`;
    for (const path of ["/auth/callback?code=example&state=community.example", "/auth/register"]) {
      const response = await fetch(base + path);
      assert.equal(response.status, 200);
      assert.match(response.headers.get("content-type"), /text\/html/);
      assert.match(await response.text(), /name="referrer" content="no-referrer"/);
    }
    assert.equal(received.length, 0);
    for (const [method, path] of [["POST", "/auth/register"], ["GET", "/auth/providers"], ["GET", "/auth/oauth/community/login"], ["POST", "/auth/oauth/community/callback"], ["POST", "/auth/oauth/register"]]) {
      const response = await fetch(base + path, { method, headers: { Origin: "https://ebs.example.com", Cookie: "test-context=opaque" } });
      assert.equal(response.status, 200);
      assert.deepEqual(received.at(-1), { path, method, cookie: "test-context=opaque", origin: "https://ebs.example.com" });
    }
  } finally {
    await vite?.close();
    gateway.closeAllConnections();
    await new Promise(resolve => gateway.close(resolve));
  }
});

test("Nginx keeps callback queries out of access logs and separates register page from POST API", () => {
  const config = readFileSync(new URL("../deploy/nginx.conf", import.meta.url), "utf8");
  assert.match(config, /add_header Referrer-Policy "no-referrer" always/);
  const locations = new Map([...config.matchAll(/location ([^\n]+) \{\n([\s\S]*?)\n    \}/g)].map(([, path, body]) => [path, body]));
  assert.match(locations.get("= /auth/callback"), /access_log off/);
  assert.match(locations.get("= /auth/callback"), /try_files \/index.html =404/);
  assert.match(locations.get("= /auth/register"), /\^\(GET\|HEAD\)\$/);
  assert.match(locations.get("= /auth/register"), /proxy_pass http:\/\/ebs-gateway:8080/);
  assert.match(locations.get("/auth/"), /proxy_next_upstream off/);
});
