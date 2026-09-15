import type { Project, ResourceList, Session, SessionIdentity } from "@/types";

const TOKEN_KEY = "eulermaker.session.token";

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly translationKey: string,
  ) {
    super(translationKey);
  }
}

export function readToken(): string {
  return sessionStorage.getItem(TOKEN_KEY) || "";
}

export function saveToken(token: string): void {
  if (token) sessionStorage.setItem(TOKEN_KEY, token);
  else sessionStorage.removeItem(TOKEN_KEY);
}

export async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  const token = readToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);
  if (init.body && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");

  const response = await fetch(path, { ...init, headers });
  if (!response.ok) throw new ApiError(response.status, errorKeyForStatus(response.status));
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export async function list<T>(path: string): Promise<{ items: T[]; next: string; remaining?: number }> {
  const response = await request<ResourceList<T>>(path);
  return {
    items: Array.isArray(response.items) ? response.items : [],
    next: response.metadata?.continue || "",
    remaining: response.metadata?.remainingItemCount,
  };
}

export async function createProject(project: Project): Promise<Project> {
  return request<Project>("/apis/ebs/v1/projects", {
    method: "POST",
    body: JSON.stringify(project),
  });
}

export async function login(username: string, password: string): Promise<Session> {
  const response = await request<{ token: string; expiresIn: number }>("/auth/login", {
    method: "POST",
    body: JSON.stringify({ username, password }),
  });
  saveToken(response.token);
  try {
    return await checkSession();
  } catch (error) {
    saveToken("");
    throw error;
  }
}

export async function checkSession(): Promise<Session> {
  const response = await request<{
    identity: SessionIdentity;
    expiresAt?: number;
  }>("/auth/check", { method: "POST" });
  return { token: readToken(), identity: response.identity, expiresAt: response.expiresAt };
}

export function errorTranslationKey(error: unknown, fallback = "errors.requestFailed"): string {
  return error instanceof ApiError ? error.translationKey : fallback;
}

function errorKeyForStatus(status: number): string {
  if (status === 400) return "errors.badRequest";
  if (status === 401) return "errors.unauthorized";
  if (status === 403) return "errors.forbidden";
  if (status === 404) return "errors.notFound";
  if (status === 409) return "errors.conflict";
  if (status === 422) return "errors.badRequest";
  if (status === 429) return "errors.rateLimited";
  return status >= 500 ? "errors.unavailable" : "errors.requestFailed";
}
