export const apiDocsMessages = {
  "zh-CN": {
    title: "API 文档",
    source: "下载 OpenAPI JSON",
    loading: "正在加载 API 文档…",
    failed: "API 文档加载失败，请稍后重试。",
    retry: "重试",
    directory: "接口目录",
    search: "搜索接口、路径或方法",
    noResults: "没有匹配的接口",
  },
  "en-US": {
    title: "API documentation",
    source: "Download OpenAPI JSON",
    loading: "Loading API documentation…",
    failed: "Could not load API documentation. Please try again.",
    retry: "Retry",
    directory: "API directory",
    search: "Search endpoints, paths or methods",
    noResults: "No matching endpoints",
  },
};

export interface ApiOperation {
  path: string;
  method: string;
  id: string;
  tag: string;
  summary: string;
}

export function apiOperations(spec: Record<string, any>): ApiOperation[] {
  const methods = new Set(["get", "post", "put", "patch", "delete", "head", "options", "trace"]);
  return Object.entries(spec.paths).flatMap(([path, item]) =>
    Object.entries(item as Record<string, any>).filter(([method]) => methods.has(method)).map(([method, operation]) => ({
      path, method, id: operation.operationId || `${method}:${path}`,
      tag: operation.tags?.[0] || "API", summary: operation.summary || path,
    })),
  );
}

export function operationHash(operation: ApiOperation): string {
  return `#/${encodeURIComponent(operation.tag)}/${encodeURIComponent(operation.id)}`;
}

export function operationDocument(spec: Record<string, any>, operation: ApiOperation): Record<string, any> {
  const item = spec.paths[operation.path];
  // Preserve path-level parameters and all component schemas referenced by the selected operation.
  const shared = Object.fromEntries(Object.entries(item).filter(([key]) =>
    ["$ref", "summary", "description", "servers", "parameters"].includes(key) || key.startsWith("x-")));
  return { ...spec, paths: { [operation.path]: { ...shared, [operation.method]: item[operation.method] } } };
}

export async function loadApiDocument(signal: AbortSignal): Promise<Record<string, unknown>> {
  // Do not use the authenticated business API client for this public document.
  const response = await fetch("/openapi.json", {
    headers: { Accept: "application/json" }, credentials: "omit", cache: "no-cache", signal,
  });
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  if (!response.headers.get("content-type")?.includes("application/json")) throw new Error("Expected JSON");
  const spec = await response.json();
  if (!spec || typeof spec.openapi !== "string" || !spec.openapi.startsWith("3.0.") || !spec.paths) {
    throw new Error("Invalid OpenAPI document");
  }
  return spec;
}

// Match the isolated document's content height so the outer page owns vertical scrolling.
export function fitApiDocsFrame(frame: HTMLIFrameElement): () => void {
  const body = frame.contentDocument?.body;
  const viewport = frame.ownerDocument.defaultView;
  if (!body || !viewport) return () => {};
  // Use the outer viewport: the iframe itself grows with the API detail content.
  const syncViewport = () => {
    frame.contentDocument!.documentElement.style.setProperty("--api-viewport-height", `${viewport.innerHeight}px`);
  };
  syncViewport();
  viewport.addEventListener("resize", syncViewport);
  const resize = () => {
    const height = `${Math.max(1, Math.ceil(body.getBoundingClientRect().height))}px`;
    if (frame.style.height !== height) frame.style.height = height;
  };
  const observer = new ResizeObserver(resize);
  observer.observe(body);
  resize();
  return () => {
    observer.disconnect();
    viewport.removeEventListener("resize", syncViewport);
  };
}

export function readOnlySwaggerOptions(spec: Record<string, unknown>, domNode: HTMLElement) {
  return {
    spec,
    domNode,
    filter: false,
    deepLinking: false,
    docExpansion: "full",
    defaultModelsExpandDepth: -1,
    supportedSubmitMethods: [],
    persistAuthorization: false,
    validatorUrl: null,
    queryConfigEnabled: false,
    // Keep security requirements in the spec; remove only credential input controls.
    plugins: [() => ({ components: { authorizeBtn: () => null, authorizeOperationBtn: () => null } })],
    // The spec is already loaded. The viewer must not fetch remote refs or execute requests.
    requestInterceptor: () => { throw new Error("API documentation is read-only"); },
  };
}
