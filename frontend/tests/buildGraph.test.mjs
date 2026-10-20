import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const source = readFileSync(new URL("../src/utils/buildGraph.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext } });
const { indexBuildGraph, constraintLabel, layoutNeighborhood, dependencyPath, isVisibleNeighbor, filterGraphNames,
  findNeighborhoodMatches } = await import(
  "data:text/javascript;base64," + Buffer.from(outputText).toString("base64")
);

test("sidebar search matches names case-insensitively and clearing restores the list", () => {
  const upstream = ["glibc", "python3", "python-PyYAML"];
  const downstream = ["rpm", "python-setuptools"];
  assert.deepEqual(filterGraphNames(upstream, " PYTHON "), ["python3", "python-PyYAML"]);
  assert.deepEqual(filterGraphNames(downstream, "rpm"), ["rpm"]);
  assert.deepEqual(filterGraphNames(upstream, ""), upstream);
  assert.deepEqual(filterGraphNames(upstream, "missing"), []);
});

test("graph find covers both full columns and the center without filtering nodes", () => {
  const upstream = Array.from({ length: 100 }, (_, i) => `python-${i}`);
  const downstream = ["python-shared", "rpm"];
  const matches = findNeighborhoodMatches("python-shared", upstream, downstream, " PYTHON ");
  assert.equal(matches.length, 102);
  assert.deepEqual(matches[99], { name: "python-99", column: "upstream", index: 99 });
  assert.deepEqual(matches[100], { name: "python-shared", column: "center", index: 0 });
  assert.deepEqual(matches[101], { name: "python-shared", column: "downstream", index: 0 });
  assert.equal(upstream.length, 100);
  assert.deepEqual(findNeighborhoodMatches("center", upstream, downstream, ""), []);
  assert.deepEqual(findNeighborhoodMatches("center", upstream, downstream, "no-match"), []);
});

test("edges point from providers to consumers and preserve build/install kinds", () => {
  const graph = indexBuildGraph({
    b: { inDep: { a: { ge: "1" } }, installInDep: { a: { lt: "3" } } },
    a: { outDep: ["b", "b"], bootstrapBreak: true },
  });
  assert.deepEqual(graph.names, ["a", "b"]);
  assert.deepEqual(graph.edges.map(({ source, target, kind }) => [source, target, kind]),
    [["a", "b", "build"], ["a", "b", "install"]]);
  assert.equal(graph.upstream.get("b").length, 2);
  assert.equal(graph.downstream.get("a").length, 2);
  assert.equal(graph.downstream.get("b").length, 0);
  assert.equal(constraintLabel(graph.edges[0].constraint), ">= 1");
});

test("cycles, self-dependencies, isolated nodes and empty graphs remain inspectable", () => {
  const graph = indexBuildGraph({
    a: { inDep: { b: {}, a: {} } },
    b: { inDep: { a: {} } },
    isolated: {},
  });
  assert.equal(graph.edges.length, 3);
  assert.equal(graph.upstream.get("a").length, 2);
  assert.equal(graph.downstream.get("a").length, 2);
  assert.deepEqual(graph.upstream.get("isolated"), []);
  assert.deepEqual(indexBuildGraph({}).names, []);
  assert.equal(constraintLabel({ ge: "1", lt: "2" }), ">= 1, < 2");
});

test("large graphs are indexed once with complete neighbor lookup", () => {
  const nodes = Object.fromEntries(Array.from({ length: 5000 }, (_, i) =>
    [`spec-${i}`, i ? { inDep: { [`spec-${i - 1}`]: {} } } : {}]));
  const graph = indexBuildGraph(nodes);
  assert.equal(graph.names.length, 5000);
  assert.equal(graph.edges.length, 4999);
  assert.equal(graph.upstream.get("spec-4999")[0].source, "spec-4998");
});

test("all direct neighbors are laid out in one independently scrolling column per side", () => {
  const upstream = Array.from({ length: 100 }, (_, i) => `provider-${i}`);
  const downstream = Array.from({ length: 21 }, (_, i) => `consumer-${i}`);
  const layout = layoutNeighborhood("selected", upstream, downstream);
  assert.equal(layout.positions.length, 122);
  for (const [side, names] of [["upstream", upstream], ["downstream", downstream]]) {
    const positions = layout.positions.filter(node => node.column === side);
    assert.deepEqual(positions.map(node => node.name), names);
    assert.equal(new Set(positions.map(node => node.x)).size, 1);
    assert.equal(new Set(positions.map(node => `${node.x},${node.y}`)).size, names.length);
    for (const node of positions) {
      assert.ok(node.x >= 0 && node.x + 240 <= layout.width);
      assert.equal(node.y, 20 + names.indexOf(node.name) * 64);
    }
  }
  const scrolled = layoutNeighborhood("selected", upstream, downstream, { upstream: 640, downstream: 128 });
  assert.deepEqual(scrolled.positions[0], layout.positions[0]);
  const left = scrolled.positions.filter(node => node.column === "upstream");
  const right = scrolled.positions.filter(node => node.column === "downstream");
  assert.equal(left[10].y, 20);
  assert.equal(right[2].y, 20);
  assert.equal(isVisibleNeighbor(left[0], scrolled.height), false);
  assert.equal(isVisibleNeighbor(left[10], scrolled.height), true);
  assert.equal(isVisibleNeighbor(left[99], scrolled.height), false);
});

test("curved edges follow scrolled endpoints while keeping the selected node fixed", () => {
  const layout = layoutNeighborhood("selected",
    Array.from({ length: 17 }, (_, i) => `provider-${i}`),
    Array.from({ length: 17 }, (_, i) => `consumer-${i}`), { upstream: 64, downstream: 128 });
  const center = layout.positions[0];
  for (const neighbor of layout.positions.slice(1)) {
    const [source, target] = neighbor.column === "upstream" ? [neighbor, center] : [center, neighbor];
    const build = dependencyPath(source, target, "build");
    const install = dependencyPath(source, target, "install");
    assert.ok(build.startsWith(`M ${source.x + 240} ${source.y + 17}`));
    assert.ok(build.endsWith(`${target.x - 3} ${target.y + 17}`));
    assert.ok(install.startsWith(`M ${source.x + 240} ${source.y + 27}`));
    assert.ok(install.endsWith(`${target.x - 3} ${target.y + 27}`));
    assert.equal(build.split(" C ").length - 1, 1);
  }
  assert.equal(layoutNeighborhood("isolated", [], []).positions.length, 1);
});

test("short neighbor columns stay vertically centered around the selected node", () => {
  const layout = layoutNeighborhood("selected", ["a"], ["b", "c", "d", "e"]);
  const center = layout.positions[0];
  for (const side of ["upstream", "downstream"]) {
    const positions = layout.positions.filter(node => node.column === side);
    const first = positions[0], last = positions.at(-1);
    assert.equal((first.y + last.y + 44) / 2, center.y + 22);
    assert.equal(first.y, 20 + layout.topPadding[side]);
  }
  assert.equal(layout.positions[1].y, center.y);
  const mixed = layoutNeighborhood("selected", ["a"], Array.from({ length: 100 }, (_, i) => `b-${i}`));
  assert.equal(mixed.positions[1].y, center.y);
  assert.equal(mixed.topPadding.downstream, 0);
});
