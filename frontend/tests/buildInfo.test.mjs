import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const source = readFileSync(new URL("../src/utils/buildInfo.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext } });
const { buildInfoSpecRows } = await import("data:text/javascript;base64," + Buffer.from(outputText).toString("base64"));

test("spec commit IDs come from their package repository in the build Snapshot", () => {
  const buildInfo = {
    status: {
      specResults: {
        "gcc/cpp.spec": { specName: "gcc-c++", status: "Running" },
        "gcc/gcc.spec": { specName: "gcc", status: "Succeeded" },
        "rpm/rpm.spec": { specName: "rpm", status: "RpmDependsMissing", message: "build requires unavailable: libc" },
      },
    },
  };
  const snapshot = { status: { packageRepoStatuses: {
    gcc: { commitID: "1234567890abcdef1234567890abcdef12345678" },
    rpm: { commitID: "abcdef1234567890abcdef1234567890abcdef12" },
  } } };
  const rows = buildInfoSpecRows(buildInfo, snapshot);
  assert.deepEqual(rows.map(row => row.name), ["gcc", "gcc-c++", "rpm"]);
  assert.equal(rows[0].commitID, snapshot.status.packageRepoStatuses.gcc.commitID);
  assert.equal(rows[1].commitID, rows[0].commitID);
  assert.equal(rows[2].commitID, snapshot.status.packageRepoStatuses.rpm.commitID);
  assert.equal(rows[2].displayStatus, "RpmDependsMissing");
  assert.equal(rows[2].buildStatus, "RpmDependsMissing");
  assert.equal(rows[2].message, "build requires unavailable: libc");
});

test("build statuses remain visible while Snapshot data is unavailable or still resolving", () => {
  const info = { status: { specResults: { "gcc/gcc.spec": { specName: "gcc", status: "Prepared" } } } };
  for (const snapshot of [null, { status: { packageRepoStatuses: { gcc: {} } } }]) {
    const [row] = buildInfoSpecRows(info, snapshot);
    assert.equal(row.name, "gcc");
    assert.equal(row.commitID, undefined);
    assert.equal(row.displayStatus, "Prepared");
  }
  assert.deepEqual(buildInfoSpecRows(null, null), []);
});

test("preparation failures keep source identity without a spec name", () => {
  const rows = buildInfoSpecRows({status: {specResults: {
    "foo/broken.spec": {status: "PrepareFailed", message: "rpmspec failed"},
    "unreadable/": {status: "PrepareFailed"},
  }}}, null);
  assert.equal(rows[0].key, "foo/broken.spec");
  assert.equal(rows[0].packageName, "foo");
  assert.equal(rows[0].specName, undefined);
  assert.equal(rows[0].message, "rpmspec failed");
  assert.equal(rows[1].packageName, "unreadable");
});

test("build failure messages are exposed directly for diagnostics", () => {
  const message = "Job creation rejected with HTTP 422";
  const [row] = buildInfoSpecRows({ status: { specResults: {
    "foo/foo.spec": { specName: "foo", status: "BuildFailed", message },
  } } }, null);
  assert.equal(row.displayStatus, "BuildFailed");
  assert.equal(row.message, message);
});
