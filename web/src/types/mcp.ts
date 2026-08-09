/** JSON-RPC 2.0 请求 */
export interface JsonRpcRequest {
  jsonrpc: "2.0";
  id: number;
  method: string;
  params?: Record<string, unknown>;
}

/** JSON-RPC 2.0 成功响应 */
export interface JsonRpcSuccessResponse {
  jsonrpc: "2.0";
  id: number;
  result: JsonRpcResult;
}

/** JSON-RPC 2.0 错误响应 */
export interface JsonRpcErrorResponse {
  jsonrpc: "2.0";
  id: number;
  error: {
    code: number;
    message: string;
  };
}

export type JsonRpcResponse = JsonRpcSuccessResponse | JsonRpcErrorResponse;

/** MCP result.content 结构 */
export interface JsonRpcResult {
  content?: Array<{
    type: "text";
    text: string;
  }>;
  /** initialize 响应专用字段 */
  protocolVersion?: string;
  capabilities?: Record<string, unknown>;
  serverInfo?: {
    name: string;
    version: string;
  };
}

/** 业务层统一响应 — 由 result.content[0].text 二次解析得到 */
export type McpToolResult = {
  ok: boolean;
  error?: string;
  [key: string]: unknown;
};
