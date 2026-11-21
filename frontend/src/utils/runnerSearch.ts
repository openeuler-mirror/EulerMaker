import type { Runner } from "@/types";
import { validRunnerLabelKey } from "./runnerLabels";

type LabelMatcher = (labels: Record<string, string>) => boolean;

export function compileRunnerLabelSelector(selector: string): LabelMatcher {
  if (!selector.trim()) return () => true;
  const checks = splitLabelTerms(selector).map((raw): LabelMatcher => {
    const term = raw.trim();
    const exists = term.match(/^(!?)([^\s!=(),]+)$/);
    if (exists && validRunnerLabelKey(exists[2])) {
      return labels => Object.hasOwn(labels, exists[2]) !== (exists[1] === "!");
    }
    const equal = term.match(/^([^\s!=(),]+)\s*(==|!=|=)\s*([^\s!=(),]*)$/);
    const set = term.match(/^([^\s!=(),]+)\s+(in|notin)\s*\(([^()]*)\)$/);
    const match = equal || set;
    if (!match || !validRunnerLabelKey(match[1])) throw new Error("invalid label selector");
    const [, key, operator, value] = match;
    const values = set ? value.split(",").map(item => item.trim()) : [value];
    if (values.some(item => item.length > 63 || (item !== "" && !/^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?$/.test(item)))) {
      throw new Error("invalid label value");
    }
    return labels => {
      const found = Object.hasOwn(labels, key) && values.includes(labels[key]);
      return operator === "!=" || operator === "notin" ? !found : found;
    };
  });
  return labels => checks.every(check => check(labels));
}

export function runnerSearchOptions(runners: Runner[]) {
  const names = new Set<string>();
  const labels = new Set<string>();
  for (const runner of runners) {
    if (runner.metadata?.name) names.add(runner.metadata.name);
    for (const [key, value] of Object.entries(runner.metadata?.labels || {})) {
      labels.add(key);
      labels.add(`${key}=${value}`);
    }
  }
  return { names: [...names].sort(), labels: [...labels].sort() };
}

function splitLabelTerms(selector: string): string[] {
  const terms: string[] = [];
  let depth = 0;
  let start = 0;
  for (let index = 0; index < selector.length; index++) {
    if (selector[index] === "(") depth++;
    else if (selector[index] === ")") depth = Math.max(0, depth - 1);
    else if (selector[index] === "," && depth === 0) {
      terms.push(selector.slice(start, index));
      start = index + 1;
    }
  }
  terms.push(selector.slice(start));
  return terms;
}

export function runnerSearchSuggestions(input: string, options: ReturnType<typeof runnerSearchOptions>): string[] {
  const markers = [...input.matchAll(/(?:^|\s+)(name|labels):\s*/g)];
  const marker = markers.at(-1);
  const prefix = input.match(/(?:^|\s+)([^\s:]*)$/);
  if (prefix) {
    const term = prefix[1];
    const before = input.slice(0, input.length - term.length);
    const fields = ["name:", "labels:"].filter(field => field.startsWith(term));
    const names = before.trim() ? [] : options.names.filter(name => name.toLowerCase().includes(term.toLowerCase()));
    if (!marker || (fields.length && !before.trimEnd().endsWith(":"))) {
      return [...fields, ...names].slice(0, 30).map(value => before + value);
    }
  }
  if (!marker) return [];
  const start = marker.index! + marker[0].length;
  let before = input.slice(0, start);
  let term = input.slice(start);
  let values = options.names;
  if (marker[1] === "labels") {
    const clauses = splitLabelTerms(term);
    term = clauses.pop()!;
    before = input.slice(0, input.length - term.length);
    // Set selectors remain editable; do not suggest replacements inside parentheses.
    if (/[()]/.test(term)) return [];
    for (let index = 0; index < markers.length - 1; index++) {
      const previous = markers[index];
      if (previous[1] === "labels") {
        clauses.push(...splitLabelTerms(input.slice(previous.index! + previous[0].length, markers[index + 1].index)));
      }
    }
    const selectedKeys = new Set(clauses.map(clause => clause.trim().match(/^!?([^\s=!,()]+)/)?.[1]).filter(Boolean));
    const spaces = term.match(/^\s*/)?.[0] || "";
    before += spaces;
    term = term.trimStart();
    values = options.labels.filter(value => !selectedKeys.has(value.split("=")[0]));
  }
  return values.filter(value => value.toLowerCase().includes(term.toLowerCase())).slice(0, 30).map(value => before + value);
}
