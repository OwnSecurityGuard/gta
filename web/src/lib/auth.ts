/**
 * 访问令牌与身份状态。
 *
 * 三个极小的可订阅 store（token / identity / authError），供
 * useSyncExternalStore 的 hook（hooks/use-auth.ts）与 mcp-client 使用：
 *  - token 持久化在 localStorage（键 gta_auth_token），无 token = 匿名模式；
 *  - identity 来自后端身份回显响应头（X-GTA-Owner / X-GTA-Admin），不持久化；
 *  - authError 在收到 401 时置位，重新保存 token 即清除。
 */

const TOKEN_KEY = "gta_auth_token";

// localStorage 在隐私模式/被禁用时可能抛异常，统一吞掉降级为内存态。
function safeGet(key: string): string | null {
  try {
    return globalThis.localStorage.getItem(key);
  } catch {
    return null;
  }
}
function safeSet(key: string, value: string): void {
  try {
    globalThis.localStorage.setItem(key, value);
  } catch {
    /* 忽略：无法持久化时仅本次会话生效 */
  }
}
function safeRemove(key: string): void {
  try {
    globalThis.localStorage.removeItem(key);
  } catch {
    /* 忽略 */
  }
}

// ===== token =====

let token: string | null = safeGet(TOKEN_KEY);

const tokenListeners = new Set<() => void>();

export function getToken(): string | null {
  return token;
}

/** 保存/清除 token；null 或空白串视为清除（回到匿名模式）。 */
export function setToken(next: string | null): void {
  const v = next?.trim() || null;
  if (v === token) return;
  token = v;
  if (v) safeSet(TOKEN_KEY, v);
  else safeRemove(TOKEN_KEY);
  // 换了凭证：旧身份与 401 状态都作废，等下一次响应头重新回显。
  setIdentity(null);
  clearAuthError();
  for (const l of tokenListeners) l();
}

export function subscribeToken(listener: () => void): () => void {
  tokenListeners.add(listener);
  return () => tokenListeners.delete(listener);
}

/** MCP 请求头：有 token 时带 Bearer，无 token 返回空对象（匿名模式零变化）。 */
export function authHeaders(): Record<string, string> {
  return token ? { Authorization: `Bearer ${token}` } : {};
}

/** SSE 等无法携带自定义头的 URL：把 token 拼进查询参数（后端 Middleware 支持回退解析）。 */
export function withTokenParam(url: string): string {
  if (!token) return url;
  return `${url}${url.includes("?") ? "&" : "?"}token=${encodeURIComponent(token)}`;
}

// ===== identity（来自响应头回显）=====

export interface Identity {
  owner: string;
  isAdmin: boolean;
}

let identity: Identity | null = null;
const identityListeners = new Set<() => void>();

export function getIdentity(): Identity | null {
  return identity;
}

export function setIdentity(next: Identity | null): void {
  if (
    identity === next ||
    (identity !== null &&
      next !== null &&
      identity.owner === next.owner &&
      identity.isAdmin === next.isAdmin)
  ) {
    return;
  }
  identity = next;
  for (const l of identityListeners) l();
}

export function subscribeIdentity(listener: () => void): () => void {
  identityListeners.add(listener);
  return () => identityListeners.delete(listener);
}

// ===== authError（401 全局横幅）=====

let authError = false;
const authErrorListeners = new Set<() => void>();

export function getAuthError(): boolean {
  return authError;
}

export function notifyAuthError(): void {
  if (authError) return;
  authError = true;
  for (const l of authErrorListeners) l();
}

export function clearAuthError(): void {
  if (!authError) return;
  authError = false;
  for (const l of authErrorListeners) l();
}

export function subscribeAuthError(listener: () => void): () => void {
  authErrorListeners.add(listener);
  return () => authErrorListeners.delete(listener);
}
