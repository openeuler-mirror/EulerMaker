export const apiDocsMessages = {
  "zh-CN": {
    title: "API 文档",
    description: "浏览 Gateway 对外接口、参数及权限要求。此页面仅供查阅，不提供在线执行。",
    source: "下载 OpenAPI JSON",
    loading: "正在加载 API 文档…",
    failed: "API 文档加载失败，请稍后重试。",
    retry: "重试",
  },
  "en-US": {
    title: "API documentation",
    description: "Browse public Gateway endpoints, parameters and permissions. This page is read-only and does not execute API calls.",
    source: "Download OpenAPI JSON",
    loading: "Loading API documentation…",
    failed: "Could not load API documentation. Please try again.",
    retry: "Retry",
  },
};

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

export function readOnlySwaggerOptions(spec: Record<string, unknown>, domNode: HTMLElement) {
  return {
    spec,
    domNode,
    filter: true,
    deepLinking: true,
    docExpansion: "none",
    defaultModelsExpandDepth: 0,
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
