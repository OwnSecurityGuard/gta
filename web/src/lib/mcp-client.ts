import type { JsonRpcRequest, JsonRpcResponse, McpToolResult } from "@/types/mcp";

/** 自增 ID 生成器 */
let nextId = 1;

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

    const response = await fetch(this.baseUrl, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(request),
    });

    if (!response.ok) {
      throw new Error(`MCP server error: ${response.status} ${response.statusText}`);
    }

    const rpcRes: JsonRpcResponse = await response.json() as JsonRpcResponse;

    if ("error" in rpcRes) {
      throw new Error(`MCP RPC error [${rpcRes.error.code}]: ${rpcRes.error.message}`);
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

    const response = await fetch(this.baseUrl, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(request),
    });

    if (!response.ok) {
      throw new Error(`MCP initialize failed: ${response.status}`);
    }
  }
}

/** 全局 MCP 客户端单例 */
export const mcpClient = new McpClient();
