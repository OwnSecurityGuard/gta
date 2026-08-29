import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  clearAuthError,
  getAuthError,
  getIdentity,
  setIdentity,
  setToken,
} from "@/lib/auth";
import { AuthError, mcpClient } from "@/lib/mcp-client";

/**
 * 构造一个够用的 Response 桩（node 环境无全局 Response 的 headers 语义细节）。
 * 与真实 Headers 一致地做大小写不敏感查找：存取两侧都归一化为小写。
 */
function jsonResponse(
  body: unknown,
  status = 200,
  headers: Record<string, string> = {},
) {
  const lowerHeaders: Record<string, string> = {};
  for (const [k, v] of Object.entries(headers)) {
    lowerHeaders[k.toLowerCase()] = v;
  }
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: status === 401 ? "Unauthorized" : "OK",
    headers: { get: (k: string) => lowerHeaders[k.toLowerCase()] ?? null },
    json: async () => body,
  };
}

/** 包一层 MCP 双层 JSON：result.content[0].text 内是业务 JSON；headers 为身份回显头。 */
function rpcOk(
  payload: Record<string, unknown>,
  headers: Record<string, string> = {},
) {
  return jsonResponse(
    {
      jsonrpc: "2.0",
      id: 1,
      result: { content: [{ type: "text", text: JSON.stringify(payload) }] },
    },
    200,
    headers,
  );
}

let fetchCalls: { url: string; init: RequestInit }[] = [];

beforeEach(() => {
  fetchCalls = [];
  vi.stubGlobal("fetch", async (url: string, init: RequestInit) => {
    fetchCalls.push({ url, init });
    return rpcOk({ ok: true });
  });
});
afterEach(() => {
  vi.unstubAllGlobals();
  setToken(null);
  // token 已为 null 时 setToken(null) 早退不清 identity，这里显式清理保证用例隔离。
  setIdentity(null);
  clearAuthError();
});

describe("callTool", () => {
  it("有 token 时请求带 Authorization: Bearer", async () => {
    setToken("gta_aaa");
    await mcpClient.callTool("list_all_sessions");
    const header = (fetchCalls[0]!.init.headers as Record<string, string>)[
      "Authorization"
    ];
    expect(header).toBe("Bearer gta_aaa");
  });

  it("无 token 时不带 Authorization 头（匿名模式零变化）", async () => {
    await mcpClient.callTool("list_all_sessions");
    expect(
      (fetchCalls[0]!.init.headers as Record<string, string>)[
        "Authorization"
      ],
    ).toBeUndefined();
  });

  it("HTTP 401 抛 AuthError 并置位全局 401 状态", async () => {
    vi.stubGlobal("fetch", async () => jsonResponse({}, 401));
    await expect(mcpClient.callTool("list_all_sessions")).rejects.toBeInstanceOf(
      AuthError,
    );
    expect(getAuthError()).toBe(true);
  });

  it("从响应头同步身份回显", async () => {
    vi.stubGlobal("fetch", async () =>
      rpcOk({ ok: true }, { "X-GTA-Owner": "bob", "X-GTA-Admin": "true" }),
    );
    await mcpClient.callTool("list_all_sessions");
    expect(getIdentity()).toEqual({ owner: "bob", isAdmin: true });
  });

  it("响应头只有 X-GTA-Owner 无 X-GTA-Admin → isAdmin 为 false", async () => {
    vi.stubGlobal("fetch", async () => rpcOk({ ok: true }, { "X-GTA-Owner": "bob" }));
    await mcpClient.callTool("list_all_sessions");
    expect(getIdentity()).toEqual({ owner: "bob", isAdmin: false });
  });

  it("成功响应无 X-GTA-Owner 时清掉旧身份", async () => {
    setIdentity({ owner: "old", isAdmin: false });
    await mcpClient.callTool("list_all_sessions"); // 默认桩不带任何回显头
    expect(getIdentity()).toBeNull();
  });

  it("网络错误（fetch reject）不置位全局 401 标志", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    await expect(mcpClient.callTool("list_all_sessions")).rejects.toThrow();
    expect(getAuthError()).toBe(false);
  });

  it("JSON-RPC error 含 unauthorized 时抛 AuthError", async () => {
    vi.stubGlobal(
      "fetch",
      async () =>
        jsonResponse({
          jsonrpc: "2.0",
          id: 1,
          error: { code: -32000, message: "unauthorized" },
        }),
    );
    await expect(mcpClient.callTool("list_all_sessions")).rejects.toBeInstanceOf(
      AuthError,
    );
  });
});

describe("initialize", () => {
  it("有 token 时请求带 Authorization: Bearer", async () => {
    setToken("gta_aaa");
    await mcpClient.initialize();
    const header = (fetchCalls[0]!.init.headers as Record<string, string>)[
      "Authorization"
    ];
    expect(header).toBe("Bearer gta_aaa");
  });

  it("HTTP 401 抛 AuthError 并置位全局 401 状态", async () => {
    vi.stubGlobal("fetch", async () => jsonResponse({}, 401));
    await expect(mcpClient.initialize()).rejects.toBeInstanceOf(AuthError);
    expect(getAuthError()).toBe(true);
  });
});
