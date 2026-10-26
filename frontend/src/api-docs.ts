import SwaggerUI from "swagger-ui-dist/swagger-ui-es-bundle.js";
import "swagger-ui-dist/swagger-ui.css";

import { apiDocsMessages, loadApiDocument, readOnlySwaggerOptions } from "@/utils/apiDocs";
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

// Keep bookmarkable operation hashes on the SPA URL while isolating Swagger styles and lifecycle.
const embedded = window.parent !== window && window.parent.location.origin === window.location.origin;
if (embedded) {
  window.history.replaceState(null, "", window.location.pathname + window.location.search + window.parent.location.hash);
  const pushState = window.history.pushState.bind(window.history);
  window.history.pushState = (data, unused, url) => {
    pushState(data, unused, url);
    window.parent.history.replaceState(window.parent.history.state, "",
      window.parent.location.pathname + window.parent.location.search + window.location.hash);
  };
}

async function load() {
  request?.abort();
  const current = new AbortController();
  request = current;
  status.textContent = messages.loading;
  retry.hidden = true;
  try {
    const spec = await loadApiDocument(current.signal);
    if (current.signal.aborted) return;
    SwaggerUI(readOnlySwaggerOptions(spec, container));
    status.textContent = "";
  } catch {
    if (current.signal.aborted) return;
    status.textContent = messages.failed;
    retry.hidden = false;
  }
}

retry.addEventListener("click", () => { void load(); });
window.addEventListener("pagehide", () => request?.abort(), { once: true });
void load();
