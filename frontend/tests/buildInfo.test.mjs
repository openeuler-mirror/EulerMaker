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
      specRepoNames: { "gcc-c++": "gcc", gcc: "gcc", rpm: "rpm" },
      specStatus: { build: {
        "gcc-c++": { status: "Processing" },
        gcc: { status: "Succeeded" },
        rpm: { status: "Failed", conditions: [{ reason: "RpmDependsMissing" }] },
      } },
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
  assert.equal(rows[2].buildStatus, "Failed");
});

test("build statuses remain visible while Snapshot data is unavailable or still resolving", () => {
  const info = { status: { specRepoNames: { gcc: "gcc" }, specStatus: { build: { gcc: { status: "Pending" } } } } };
  for (const snapshot of [null, { status: { packageRepoStatuses: { gcc: {} } } }]) {
    const [row] = buildInfoSpecRows(info, snapshot);
    assert.equal(row.name, "gcc");
    assert.equal(row.commitID, undefined);
    assert.equal(row.displayStatus, "Pending");
  }
  assert.deepEqual(buildInfoSpecRows(null, null), []);
});
