import type { BuildGraphNode, VersionConstraint } from "../types";

export function filterGraphNames(names: string[], search: string): string[] {
  const query = search.trim().toLowerCase();
  return names.filter(name => name.toLowerCase().includes(query));
}

export function findNeighborhoodMatches(selected: string, upstream: string[], downstream: string[], search: string) {
  const query = search.trim().toLowerCase();
  if (!query) return [];
  return [
    ...upstream.map((name, index) => ({ name, index, column: "upstream" as const })),
    { name: selected, index: 0, column: "center" as const },
    ...downstream.map((name, index) => ({ name, index, column: "downstream" as const })),
  ].filter(node => node.name.toLowerCase().includes(query));
}

export interface DependencyEdge {
  source: string;
  target: string;
  kind: "build" | "install";
  constraint: VersionConstraint;
}

// Index each dependency kind separately: the same pair may have both kinds of edge.
export function indexBuildGraph(nodes: Record<string, BuildGraphNode>) {
  const names = Object.keys(nodes).sort((a, b) => a.localeCompare(b));
  const upstream = new Map<string, DependencyEdge[]>(names.map(name => [name, []]));
  const downstream = new Map<string, DependencyEdge[]>(names.map(name => [name, []]));
  const edges: DependencyEdge[] = [];
  for (const target of names) {
    for (const kind of ["build", "install"] as const) {
      const deps = kind === "build" ? nodes[target].inDep : nodes[target].installInDep;
      for (const source of Object.keys(deps || {}).sort()) {
        if (!downstream.has(source)) continue;
        const edge = { source, target, kind, constraint: deps![source] };
        edges.push(edge);
        upstream.get(target)!.push(edge);
        downstream.get(source)!.push(edge);
      }
    }
  }
  return { names, edges, upstream, downstream };
}

export function constraintLabel(constraint: VersionConstraint): string {
  return (Object.entries({ gt: ">", ge: ">=", eq: "=", le: "<=", lt: "<" }) as Array<[keyof VersionConstraint, string]>)
    .filter(([key]) => constraint[key])
    .map(([key, operator]) => `${operator} ${constraint[key]}`)
    .join(", ");
}

export interface GraphPosition {
  name: string;
  column: "upstream" | "center" | "downstream";
  x: number;
  y: number;
}

// Each side scrolls independently while the selected node stays centered.
export function layoutNeighborhood(selected: string, upstream: string[], downstream: string[],
  scroll = { upstream: 0, downstream: 0 }) {
  // Show 12 rows (44px each, 20px gaps) with 20px padding above and below.
  const height = 12 * 44 + 11 * 20 + 40;
  const padding = (count: number) => Math.max(0, (height - 40 - Math.max(0, count * 64 - 20)) / 2);
  const topPadding = { upstream: padding(upstream.length), downstream: padding(downstream.length) };
  const positions: GraphPosition[] = [{ name: selected, column: "center", x: 380, y: height / 2 - 22 }];
  for (const [column, names, startX] of [
    ["upstream", upstream, 20],
    ["downstream", downstream, 740],
  ] as const) {
    names.forEach((name, i) => positions.push({
      name, column, x: startX, y: 20 + topPadding[column] + i * 64 - scroll[column],
    }));
  }
  return { positions, width: 1000, height, topPadding };
}

export function isVisibleNeighbor(position: GraphPosition, height: number): boolean {
  return position.y + 17 >= 20 && position.y + 27 <= height - 20;
}

export function dependencyPath(source: GraphPosition, target: GraphPosition, kind: DependencyEdge["kind"]): string {
  const offset = kind === "install" ? 5 : -5;
  const startX = source.x + 240, startY = source.y + 22 + offset;
  const endX = target.x - 3, endY = target.y + 22 + offset;
  const controlX = (startX + endX) / 2;
  return `M ${startX} ${startY} C ${controlX} ${startY}, ${controlX} ${endY}, ${endX} ${endY}`;
}
