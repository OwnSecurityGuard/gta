/**
 * get_session_timeline 返回的会话时间线类型。
 *
 * 时间线以事件为节点、TraceContext.CausationID 建立父子树（OpenTelemetry 的 parent span）。
 * 当解码器/协议解析器产出了通信语义（_meta.protocol）时，节点额外携带 `proto`；
 * 无语义的事件即普通 JSON 事件，前端自动降级为 Raw JSON 展示（语义为增强信息，不改变原始数据）。
 */

/** 固定语义状态机。颜色映射由前端统一决定，不在各处猜。 */
export type SemanticStatus =
  | "normal" /** 普通消息（Push / 无语义 fallback） */
  | "success" /** Response 成功 */
  | "warning" /** Timeout / 无响应 */
  | "error" /** Response 失败 */
  | "pending" /** Request 等待响应 */
  | "unknown"; /** Unknown Message */

/** _meta.protocol 中投影的通信语义（与后端 TimelineProtocol 对应）。 */
export interface TimelineProtocol {
  /** 语义消息名，如 LoginRequest */
  message?: string;
  /** 角色：request | response | push | unknown */
  role?: string;
  /** 投递类型（request_response / server_push 等），协议配置产物 */
  delivery?: string;
  /** Request/Response 关联信息 */
  correlation?: TimelineCorrelation;
  /** Response 错误语义 */
  error?: TimelineError;
}

/** 一条消息的 Request/Response 关联。 */
export interface TimelineCorrelation {
  direction?: string; // request | response
  rule?: string;
  key?: string;
  value?: string;
}

/** Response 错误语义。 */
export interface TimelineError {
  failed: boolean;
  code?: string;
}

/** 时间线树的一个节点，对应一条已解码事件。 */
export interface TimelineNode {
  id: string;
  timestamp: string;
  schema_id?: string;
  /** 事件类型（identity.type，如 request/response 等） */
  type?: string;
  msg_name?: string;
  /** 消息方向：client_to_server | server_to_client | "" */
  direction?: string;
  correlation_id?: string;
  is_push?: boolean;
  /** 通信语义（有则增强展示，无则降级为 Raw JSON） */
  proto?: TimelineProtocol;
  /** 干净业务 JSON（不含 _meta），供 Raw JSON 视图 */
  json?: string;
  /** 事件摘要（payload 的截断 JSON 文本） */
  summary?: string;
  /** 因果子节点（causation 指向本节点的事件） */
  children?: TimelineNode[];
}

/** 同一 correlation_id 下事件的聚合视图。 */
export interface ConversationView {
  correlation_id: string;
  event_count: number;
}

/** get_session_timeline 完整响应。 */
export interface SessionTimelineResult {
  ok: boolean;
  session_id: string;
  plugin?: string;
  status?: string;
  event_count: number;
  root_count: number;
  conversations?: ConversationView[];
  roots: TimelineNode[];
  uncertainties?: string[];
}