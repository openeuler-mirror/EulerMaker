export interface LoginProvider { id: string; loginURL: string }
export interface LoginMethods {
  passwordLoginEnabled: boolean;
  registrationEnabled: boolean;
  providers: LoginProvider[];
}
export interface OAuthLogin { token: string; tokenType: string; expiresIn: number; returnTo: string }
export interface RegistrationContext {
  registrationRequired: true;
  registrationID: string;
  expiresAt: string;
  profile: { displayName?: string; email?: string };
}
export interface RegistrationDraft extends RegistrationContext {
  username: string;
  displayName: string;
  email: string;
  submitted: boolean;
}
export class OAuthError extends Error {
  constructor(readonly code: string, readonly status = 0) { super(code); }
}

const registrationKey = "eulermaker.oauth.registration";
const providerPattern = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

export async function oauthRequest<T>(path: string, body?: object, signal?: AbortSignal): Promise<T> {
  const response = await fetch(path, {
    method: body ? "POST" : "GET",
    headers: body ? { Accept: "application/json", "Content-Type": "application/json" } : { Accept: "application/json" },
    credentials: "same-origin", cache: "no-store", redirect: "error",
    body: body ? JSON.stringify(body) : undefined,
    signal: signal || AbortSignal.timeout(35000),
  });
  const data = await response.json().catch(() => null);
  if (!response.ok) throw new OAuthError(data?.code || "OAuthUnavailable", response.status);
  if (!data) throw new OAuthError("OAuthUnavailable");
  return data as T;
}

export function providerLoginURL(provider: LoginProvider, returnTo: unknown): string {
  if (!providerPattern.test(provider.id) || provider.loginURL !== `/auth/oauth/${provider.id}/login`) {
    throw new OAuthError("ProviderUnavailable");
  }
  return `${provider.loginURL}?${new URLSearchParams({ returnTo: safeReturnTo(returnTo) })}`;
}

export function safeReturnTo(value: unknown): string {
  if (typeof value !== "string" || !value.startsWith("/") || /[\\%#\x00-\x20]/.test(value)) return "/projects";
  const path = value.split("?")[0];
  if (["/", "/projects", "/operations", "/runners", "/settings", "/api-docs", "/admin", "/admin/users", "/admin/machineaccounts"].includes(path)
    || /^\/projects\/[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(path)) return value;
  return "/projects";
}

interface CallbackInput { provider: string; body: Record<string, string> }
let callback: CallbackInput | OAuthError | null = null;

export function parseCallback(search: string): CallbackInput {
  const params = new URLSearchParams(search);
  for (const key of ["code", "state", "error", "iss"]) {
    if (params.getAll(key).length > 1) throw new OAuthError("InvalidRequest");
  }
  const state = params.get("state") || "";
  const provider = state.split(".")[0];
  if (!providerPattern.test(provider) || !state.includes(".") || !state.split(".")[1]
    || params.has("code") === params.has("error") || !(params.get("code") || params.get("error"))) {
    throw new OAuthError("InvalidRequest");
  }
  const body: Record<string, string> = { state };
  for (const key of ["code", "error", "iss"]) if (params.has(key)) body[key] = params.get(key)!;
  return { provider, body };
}

// Capture once before session restoration or routing; never persist authorization codes.
export function captureOAuthCallback(): void {
  if (window.location.pathname !== "/auth/callback") return;
  const search = window.location.search;
  window.history.replaceState(window.history.state, "", "/auth/callback");
  try { callback = parseCallback(search); }
  catch { callback = new OAuthError("InvalidRequest"); }
}

export function takeOAuthCallback(): CallbackInput {
  const input = callback;
  callback = null;
  if (!input) throw new OAuthError("ContextExpired");
  if (input instanceof OAuthError) throw input;
  return input;
}

export function clearRegistration(): void { sessionStorage.removeItem(registrationKey); }

export function saveRegistration(draft: RegistrationDraft): void {
  try { sessionStorage.setItem(registrationKey, JSON.stringify(draft)); }
  catch { throw new OAuthError("StorageUnavailable"); }
}

export function beginRegistration(context: RegistrationContext): void {
  const draft: RegistrationDraft = {
    ...context, username: "", displayName: context.profile?.displayName || "", email: context.profile?.email || "", submitted: false,
  };
  if (!validDraft(draft)) throw new OAuthError("ContextExpired");
  saveRegistration(draft);
}

function validDraft(value: RegistrationDraft): boolean {
  return value?.registrationRequired === true && typeof value.registrationID === "string" && !!value.registrationID
    && typeof value.expiresAt === "string" && Date.parse(value.expiresAt) > Date.now()
    && typeof value.username === "string" && typeof value.displayName === "string" && typeof value.email === "string"
    && typeof value.submitted === "boolean" && (!value.submitted || providerPattern.test(value.username));
}

export function readRegistration(): RegistrationDraft | null {
  try {
    const value = JSON.parse(sessionStorage.getItem(registrationKey) || "null") as RegistrationDraft;
    if (validDraft(value)) return value;
    clearRegistration();
  } catch { /* Missing or unavailable browser storage requires a new login. */ }
  return null;
}

export function oauthErrorKey(error: unknown): string {
  const keys: Record<string, string> = {
    OAuthCancelled: "oauth.cancelled", ContextExpired: "oauth.expired", InvalidRequest: "oauth.invalidRequest",
    OAuthConfigChanged: "oauth.expired", BrowserBindingInvalid: "oauth.expired", StorageUnavailable: "oauth.storageUnavailable",
    UsernameAlreadyExists: "auth.usernameTaken", RegistrationInProgress: "oauth.unknownResult",
    IdentityAlreadyLinked: "oauth.identityLinked", RateLimited: "errors.rateLimited",
    OAuthAuthenticationFailed: "oauth.authenticationFailed", ProviderUnavailable: "oauth.unavailable",
  };
  return error instanceof OAuthError ? (keys[error.code] || "oauth.unavailable") : "oauth.unavailable";
}

export function registrationErrorAction(error: unknown): "edit" | "restart" | "confirm" {
  if (error instanceof OAuthError) {
    if ((error.status === 400 && error.code === "InvalidRequest")
      || (error.status === 409 && error.code === "UsernameAlreadyExists")) return "edit";
    if (["ContextExpired", "OAuthConfigChanged", "BrowserBindingInvalid", "IdentityAlreadyLinked", "ProviderUnavailable", "OAuthAuthenticationFailed"].includes(error.code)) return "restart";
  }
  return "confirm";
}
