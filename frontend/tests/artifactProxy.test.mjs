import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import http from "node:http";
import { fileURLToPath } from "node:url";
import test from "node:test";

const configPath = fileURLToPath(new URL("../deploy/nginx.conf", import.meta.url));
const config = readFileSync(configPath, "utf8");
const locations = [...config.matchAll(/location ([^\n]+) \{\n([\s\S]*?)\n    \}/g)];

test("both artifact proxy routes configure bounded streaming uploads and timeouts", () => {
  const routes = locations.filter(([, route]) => route.includes("artifacts|repositories"));
  assert.equal(routes.length, 2);
  for (const [, , body] of routes) {
    for (const directive of [
      "client_max_body_size 2g;", "client_body_timeout 300s;",
      "proxy_request_buffering off;", "proxy_buffering off;", "proxy_http_version 1.1;",
      "proxy_connect_timeout 10s;", "proxy_send_timeout 300s;",
      "proxy_read_timeout 7200s;", "send_timeout 300s;", "proxy_next_upstream off;",
    ]) assert.ok(body.includes(directive), directive);
  }
  for (const [, route, body] of locations.filter(([, route]) => ["/auth/", "/apis/"].includes(route))) {
    assert.ok(!body.includes("client_max_body_size 2g"), `${route} keeps its existing size limit`);
  }
});

const nginxImage = process.env.NGINX_TEST_IMAGE;
test("Nginx streams Runner uploads on default and instance-prefixed routes", {
  skip: !nginxImage && "set NGINX_TEST_IMAGE to a locally available frontend image",
  timeout: 60000,
}, async () => {
  const docker = (...args) => execFileSync("docker", args, { encoding: "utf8", timeout: 15000 }).trim();
  const id = `ebs-proxy-test-${randomUUID()}`;
  const upstream = `${id}-upstream`;
  const proxy = `${id}-nginx`;
  const containers = [];
  const delay = () => new Promise(resolve => setTimeout(resolve, 100));
  const waitFor = async (check) => {
    for (let attempt = 0; attempt < 80; attempt++) {
      if (await check().catch(() => false)) return;
      await delay();
    }
    throw new Error("proxy or streaming upload did not become ready");
  };
  docker("network", "create", id);
  try {
    docker("run", "-d", "--name", upstream, "--network", id,
      "--network-alias", "ebs-gateway", "--network-alias", "artifact-manager",
      "--network-alias", "artifact-manager.art-01.svc.cluster.local",
      "node:22-alpine", "node", "-e", `
        const http = require('node:http');
        const received = new Map();
        http.createServer((req, res) => {
          const id = req.headers['x-test-id'] || req.url.split('/').pop();
          res.setHeader('Content-Type', 'application/json');
          if (req.url.startsWith('/repositories/probe/')) {
            return res.end(JSON.stringify({bytes: received.get(id) || 0}));
          }
          if (req.headers.authorization !== 'Bearer test-token') {
            res.writeHead(401);
            return res.end(JSON.stringify({code: 'Unauthorized'}));
          }
          let bytes = 0;
          req.on('data', data => { bytes += data.length; received.set(id, bytes); });
          req.on('end', () => res.end(JSON.stringify({url: req.url, headers: req.headers, bytes})));
        }).listen(8081, '0.0.0.0');
      `);
    containers.push(upstream);
    docker("run", "-d", "--name", proxy, "--network", id,
      "-p", "127.0.0.1::8080", "-e", "DEFAULT_ARTIFACT_MANAGER=artifact-manager:8081",
      "-e", "NGINX_DNS_RESOLVER=127.0.0.11",
      "-v", `${configPath}:/etc/nginx/templates/default.conf.template:ro`, nginxImage);
    containers.push(proxy);
    const address = `http://${docker("port", proxy, "8080/tcp")}`;
    await waitFor(async () => (await fetch(`${address}/healthz`)).ok);
    docker("exec", proxy, "nginx", "-t");

    for (const prefix of ["", "/art-01"]) {
      for (const [suffix, size, contentType] of [
        ["artifacts", 2 * 1024 * 1024, "multipart/form-data; boundary=test-boundary"],
        ["logs/chunks", 256 * 1024, "application/octet-stream"],
      ]) {
        const path = `/artifacts/v1/projects/test/jobs/job/${suffix}`;
        const uploadID = randomUUID();
        let req;
        const response = new Promise((resolve, reject) => {
          req = http.request(`${address}${prefix}${path}`, { method: "POST", headers: {
            Authorization: "Bearer test-token", "Content-Type": contentType,
            "Idempotency-Key": "test-key", "X-Log-Stream": "combined",
            "X-Log-Sequence": "0", "X-Content-SHA256": "test-sha256",
            "X-Test-ID": uploadID,
          } }, res => {
            let body = "";
            res.on("data", data => { body += data; });
            res.on("end", () => resolve({ status: res.statusCode, body: JSON.parse(body) }));
            res.on("error", reject);
          });
          req.on("error", reject);
        });
        response.catch(() => {});
        try {
          const start = suffix === "artifacts" ? '--test-boundary\r\nContent-Disposition: form-data; name="file"; filename="test.rpm"\r\n\r\n' : "";
          const end = suffix === "artifacts" ? "\r\n--test-boundary--\r\n" : "end";
          const data = Buffer.concat([Buffer.from(start), Buffer.alloc(size, "x")]);
          req.write(data); // No Content-Length: exercise the Runner's chunked streaming path.
          await waitFor(async () => {
            const result = await fetch(`${address}${prefix}/repositories/probe/${uploadID}`);
            return (await result.json()).bytes === data.length;
          });
          req.end(end);
          const result = await response;
          assert.equal(result.status, 200);
          assert.equal(result.body.url, path);
          assert.equal(result.body.bytes, data.length + Buffer.byteLength(end));
          for (const [header, value] of Object.entries({
            authorization: "Bearer test-token", "idempotency-key": "test-key",
            "x-log-stream": "combined", "x-log-sequence": "0", "x-content-sha256": "test-sha256",
          })) assert.equal(result.body.headers[header], value);
        } finally {
          req.destroy();
        }
      }
      for (const [method, suffix] of [
        ["POST", "logs/complete"], ["POST", "manifest/complete"],
        ["GET", "logs/status?stream=combined"], ["GET", "manifest"],
      ]) {
        const path = `/artifacts/v1/projects/test/jobs/job/${suffix}`;
        const result = await fetch(`${address}${prefix}${path}`, {
          method, headers: { Authorization: "Bearer test-token", "Content-Type": "application/json" },
          ...(method === "POST" ? { body: "{}" } : {}),
        });
        assert.equal(result.status, 200);
        assert.equal((await result.json()).url, path);
      }
      const rejected = await fetch(`${address}${prefix}/artifacts/v1/projects/test/jobs/job/artifacts`, { method: "POST" });
      assert.equal(rejected.status, 401);
      assert.equal((await rejected.json()).code, "Unauthorized");
    }
  } finally {
    for (const container of containers.reverse()) docker("rm", "-f", container);
    docker("network", "rm", id);
  }
});
