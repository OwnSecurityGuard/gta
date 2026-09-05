import type { JsonRpcRequest, JsonRpcResponse, McpToolResult } from "@/types/mcp";
import { authHeaders, getToken, notifyAuthError, setIdentity } from "@/lib/auth";

/** 自增 ID 生成器 */
let nextId = 1;

/** 服务器开启 token 校验而本地未携带/凭证失效时抛出；App 层据此弹出设置引导。 */
export class AuthError extends Error {
  constructor(message = "需要访问令牌（HTTP 401）") {
    super(message);
    this.name = "AuthError";
  }
}

/** 从响应头读取身份回显（后端 auth.Middleware 注入；匿名模式无此头 → 清空身份）。 */
function syncIdentityFromHeaders(headers: {
  get(k: string): string | null;
}): void {
  const owner = headers.get("X-GTA-Owner");
  if (!owner) {
    setIdentity(null);
    return;
  }
  setIdentity({ owner, isAdmin: headers.get("X-GTA-Admin") === "true" });
}

/**
 * MCP JSON-RPC 客户端
 * 与 game-traffic-analysis 的 POST /mcp 端点通信
 */
export class McpClient {
  private baseUrl: string;

  constructor(baseUrl = "/mcp") {
    this.baseUrl = baseUrl;
  }

  setBaseUrl(url: string) {
    this.baseUrl = url;
  }

  getBaseUrl(): string {
    return this.baseUrl;
  }

  /**
   * 自助注册：完全的新用户免邀请获取独立身份（POST /access/register）。
   * 返回的 token 与邀请凭证同待遇 —— 仅此一次展示，由调用方立即保存。
   * 服务端关闭注册（GTA_AUTH_REGISTER=off / 匿名模式）时返回 403。
   */
  async register(name: string): Promise<{ owner: string; token: string }> {
    // baseUrl 形如 "/mcp"（走同源代理）或 "http://host:8781/mcp"（直连）：
    // 注册端点挂在同源根路径 /access/register 上，这里还原出 origin 再拼接。
    const u = new URL(this.baseUrl, window.location.origin);
    const res = await fetch(`${u.origin}/access/register`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    });
    if (!res.ok) {
      const text = await res.text().catch(() => "");
      throw new Error(text || `注册失败（HTTP ${res.status}）`);
    }
    const data = (await res.json()) as { ok: boolean; owner: string; token: string };
    if (!data.ok || !data.token) {
      throw new Error("注册响应缺少 token");
    }
    return { owner: data.owner, token: data.token };
  }

  /**
   * 发送 JSON-RPC 请求并返回业务层结果
   * 自动处理 MCP 协议的双层 JSON 解析 (response → content[0].text)
   */
  async callTool<T = McpToolResult>(
    name: string,
    args: Record<string, unknown> = {},
  ): Promise<T> {
    const id = nextId++;
    const request: JsonRpcRequest = {
      jsonrpc: "2.0",
      id,
      method: "tools/call",
      params: {
        name,
        arguments: args,
      },
    };

    // 记下发请求时用的 token：在途期间 token 可能被更换，401 归属判断要用。
    const usedToken = getToken();
    const response = await fetch(this.baseUrl, {
      method: "POST",
      headers: { "Content-Type": "application/json", ...authHeaders() },
      body: JSON.stringify(request),
    });

    if (response.status === 401) {
      // 在途请求可能来自已更换的旧凭证：仅当 401 属于当前 token 时才置位横幅，
      // 否则保存新 token 后会被旧请求的迟到 401 重新点亮且无法自愈。
      if (usedToken === getToken()) {
        setIdentity(null);
        notifyAuthError();
      }
      throw new AuthError();
    }
    if (!response.ok) {
      throw new Error(`MCP server error: ${response.status} ${response.statusText}`);
    }
    syncIdentityFromHeaders(response.headers);

    const rpcRes: JsonRpcResponse = await response.json() as JsonRpcResponse;

    if ("error" in rpcRes) {
      const msg = String(rpcRes.error.message ?? "");
      // \b 防止误伤：如 "4014"、":40180" 这类含 401 子串的无关错误消息。
      if (/\bunauthorized\b|\b401\b/i.test(msg)) {
        notifyAuthError();
        throw new AuthError(msg);
      }
      throw new Error(`MCP RPC error [${rpcRes.error.code}]: ${msg}`);
    }

    // 提取 content[0].text 并二次解析
    const content = rpcRes.result?.content;
    if (!content || !content[0]?.text) {
      throw new Error("MCP server returned empty content");
    }

    const parsed: McpToolResult = JSON.parse(content[0].text) as McpToolResult;

    if (!parsed.ok) {
      throw new Error(parsed.error ?? "MCP tool returned ok=false");
    }

    return parsed as T;
  }

  /**
   * 初始化 MCP 连接（可选，stateless 模式下非必须）
   */
  async initialize(): Promise<void> {
    const id = nextId++;
    const request: JsonRpcRequest = {
      jsonrpc: "2.0",
      id,
      method: "initialize",
      params: {
        protocolVersion: "2024-11-05",
        capabilities: {},
        clientInfo: {
          name: "game-traffic-analysis-web",
          version: "0.1.0",
        },
      },
    };

    // 记下发请求时用的 token：在途期间 token 可能被更换，401 归属判断要用。
    const usedToken = getToken();
    const response = await fetch(this.baseUrl, {
      method: "POST",
      headers: { "Content-Type": "application/json", ...authHeaders() },
      body: JSON.stringify(request),
    });

    if (response.status === 401) {
      // 在途请求可能来自已更换的旧凭证：仅当 401 属于当前 token 时才置位横幅，
      // 否则保存新 token 后会被旧请求的迟到 401 重新点亮且无法自愈。
      if (usedToken === getToken()) {
        setIdentity(null);
        notifyAuthError();
      }
      throw new AuthError();
    }
    if (!response.ok) {
      throw new Error(`MCP initialize failed: ${response.status}`);
    }
    syncIdentityFromHeaders(response.headers);
  }
}

/** 全局 MCP 客户端单例 */
export const mcpClient = new McpClient();
