import SwaggerUI from "swagger-ui-dist/swagger-ui-es-bundle.js";
import "swagger-ui-dist/swagger-ui.css";

import { apiDocsMessages, apiOperations, loadApiDocument, operationDocument, operationHash, readOnlySwaggerOptions } from "@/utils/apiDocs";
import type { ApiOperation } from "@/utils/apiDocs";
import "@/styles/api-docs.css";

const language = new URLSearchParams(window.location.search).get("lang") === "zh-CN" ? "zh-CN" : "en-US";
const messages = apiDocsMessages[language];
document.documentElement.lang = language;
document.title = messages.title;
const status = document.querySelector<HTMLElement>("#status")!;
const retry = document.querySelector<HTMLButtonElement>("#retry")!;
const container = document.querySelector<HTMLElement>("#documentation")!;
retry.textContent = messages.retry;
let request: AbortController | undefined;
const layout = document.querySelector<HTMLElement>("#api-layout")!;
const sidebar = document.querySelector<HTMLElement>("#sidebar")!;
const directory = document.querySelector<HTMLElement>("#api-directory")!;
const search = document.querySelector<HTMLInputElement>("#api-search")!;
const toggle = document.querySelector<HTMLButtonElement>("#directory-toggle")!;
const noResults = document.querySelector<HTMLElement>("#no-results")!;
document.querySelector("#search-label")!.textContent = messages.directory;
const download = document.querySelector<HTMLAnchorElement>("#api-docs-download")!;
download.title = messages.source;
download.setAttribute("aria-label", messages.source);
search.placeholder = messages.search;
search.setAttribute("aria-label", messages.search);
directory.setAttribute("aria-label", messages.directory);
toggle.textContent = messages.directory;
noResults.textContent = messages.noResults;
let spec: Record<string, unknown>;
let operations: ApiOperation[] = [];
let selected: ApiOperation | undefined;
let viewer: ReturnType<typeof SwaggerUI> | undefined;

// The directory owns deep links; Swagger renders only the selected operation.
const embedded = window.parent !== window && window.parent.location.origin === window.location.origin;
const navigation = embedded ? window.parent : window;

function selectOperation(operation: ApiOperation) {
  if (selected?.id === operation.id) return;
  selected = operation;
  const single = operationDocument(spec, operation);
  if (viewer) viewer.specActions.updateJsonSpec(single);
  else viewer = SwaggerUI(readOnlySwaggerOptions(single, container));
  for (const link of directory.querySelectorAll<HTMLAnchorElement>("a[data-operation]")) {
    if (link.dataset.operation === operation.id) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  }
}

function restoreSelection() {
  const operation = operations.find(item => operationHash(item) === navigation.location.hash) || operations[0];
  if (operation) selectOperation(operation);
}

function renderDirectory() {
  directory.replaceChildren();
  const query = search.value.trim().toLowerCase();
  const matches = operations.filter(item =>
    `${item.tag} ${item.summary} ${item.method} ${item.path} ${item.id}`.toLowerCase().includes(query));
  const groups = new Map<string, HTMLElement>();
  for (const operation of matches) {
    let group = groups.get(operation.tag);
    if (!group) {
      group = document.createElement("section");
      const title = document.createElement("h2");
      title.textContent = operation.tag;
      group.append(title);
      groups.set(operation.tag, group);
      directory.append(group);
    }
    const link = document.createElement("a");
    link.href = navigation.location.pathname + navigation.location.search + operationHash(operation);
    link.dataset.operation = operation.id;
    link.title = `${operation.method.toUpperCase()} ${operation.path}`;
    if (operation.id === selected?.id) link.setAttribute("aria-current", "page");
    const method = document.createElement("span");
    method.className = `api-method method-${operation.method}`;
    method.textContent = operation.method.toUpperCase();
    const label = document.createElement("span");
    label.className = "api-operation-label";
    const summary = document.createElement("span");
    summary.textContent = operation.summary;
    const path = document.createElement("small");
    path.textContent = operation.path;
    label.append(summary, path);
    link.append(method, label);
    link.addEventListener("click", event => {
      if (event.ctrlKey || event.metaKey || event.shiftKey || event.altKey || event.button !== 0) return;
      event.preventDefault();
      navigation.location.hash = operationHash(operation);
      selectOperation(operation);
      sidebar.classList.remove("is-open");
      toggle.setAttribute("aria-expanded", "false");
    });
    // Open copied links in the outer app rather than the isolated viewer.
    if (embedded) link.target = "_parent";
    group.append(link);
  }
  noResults.hidden = matches.length > 0;
}

async function load() {
  request?.abort();
  const current = new AbortController();
  request = current;
  status.textContent = messages.loading;
  retry.hidden = true;
  try {
    spec = await loadApiDocument(current.signal);
    if (current.signal.aborted) return;
    operations = apiOperations(spec);
    layout.hidden = false;
    renderDirectory();
    restoreSelection();
    status.textContent = "";
  } catch {
    if (current.signal.aborted) return;
    status.textContent = messages.failed;
    retry.hidden = false;
  }
}

retry.addEventListener("click", () => { void load(); });
search.addEventListener("input", renderDirectory);
toggle.addEventListener("click", () => {
  const open = sidebar.classList.toggle("is-open");
  toggle.setAttribute("aria-expanded", String(open));
});
navigation.addEventListener("hashchange", restoreSelection);
navigation.addEventListener("popstate", restoreSelection);
window.addEventListener("pagehide", () => {
  request?.abort();
  navigation.removeEventListener("hashchange", restoreSelection);
  navigation.removeEventListener("popstate", restoreSelection);
}, { once: true });
void load();
