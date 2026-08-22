/**
 * get_session_timeline 返回的会话时间线类型。
 *
 * 时间线以事件为节点、TraceContext.CausationID 建立父子树（OpenTelemetry 的 parent span），
 * 同一 correlation_id 的事件聚合为一个"对话/请求-响应分组"（ConversationView）。
 */

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
