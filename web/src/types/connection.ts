/** 捕获上下文（Capture Context）：代理抓包特有的事件归属信息。
 * 由后端 list_decoded_data 在每个事件的 `capture` 字段返回，前端据此展示
 * "Captured By / Connection / Stream / Source"。 */
export interface CaptureContext {
  /** 抓包来源展示名，如 "Mobile Proxy" */
  captured_by: string;
  /** 连接 ID（移动代理的 conn_id） */
  conn_id: string;
  /** 连接序号（1-based，按连接最新事件时间倒序，最新为 1） */
  conn_seq: number;
  /** 流分组键（correlation_id 或事件 ID） */
  stream_id: string;
  /** 连接内流序号（1-based，按流首事件时间正序） */
  stream_seq: number;
  /** 抓包来源原始值（如 mobile） */
  source: string;
}

/** list_connections 返回的一行（Connections 列表）。 */
export interface ConnectionSummary {
  conn_id: string;
  client: string;
  server: string;
  /** 原始网络协议（如 tcp） */
  protocol: string;
  /** 连接内首个解码事件类型（如 http_req），可用于展示 HTTPS/HTTP */
  event_type: string;
  /** 抓包来源（mobile / pcap-live / ...） */
  source: string;
  start_time: string;
  end_time: string;
  duration_sec: number;
  event_count: number;
  frame_count: number;
}

/** get_connection_detail 返回的连接详情（Detail 头部 + 统计）。 */
export interface ConnectionDetail {
  conn_id: string;
  client: string;
  server: string;
  protocol: string;
  event_type: string;
  source: string;
  app: string;
  device: string;
  start_time: string;
  end_time: string;
  duration_sec: number;
  event_count: number;
  stream_count: number;
  frame_count: number;
}

/** 连接内单个解码事件（Stream View 与 Events 子页共用）。 */
export interface ConnectionEvent {
  id: string;
  timestamp: string;
  type: string;
  /** client_to_server | server_to_client | "" */
  direction: string;
  correlation_id: string;
  flow_id: string;
  msg_name: string;
  data: Record<string, unknown>;
}

/** 连接内一条流（按 correlation_id 分组；未关联事件各自成流）。 */
export interface ConnectionStream {
  /** 流序号（1-based，按首事件时间排序） */
  seq: number;
  key: string;
  /** 非空表示该流来自关联对话（request/response 配对） */
  correlation_id: string;
  start_time: string;
  end_time: string;
  event_count: number;
  events: ConnectionEvent[];
}

/** 连接内原始帧（Frames / Raw 子页共用）。 */
export interface ConnectionFrame {
  id: string;
  timestamp: string;
  direction: string;
  src: string;
  dst: string;
  protocol: string;
  link_type: number;
  /** base64 编码的原始负载 */
  payload: string;
}

/** list_connections 完整响应。 */
export interface ListConnectionsResult {
  ok: boolean;
  count: number;
  connections: ConnectionSummary[];
}

/** get_connection_detail 完整响应。 */
export interface GetConnectionDetailResult {
  ok: boolean;
  connection: ConnectionDetail;
}

/** list_connection_streams 完整响应。 */
export interface ListConnectionStreamsResult {
  ok: boolean;
  count: number;
  streams: ConnectionStream[];
}

/** list_connection_frames 完整响应。 */
export interface ListConnectionFramesResult {
  ok: boolean;
  count: number;
  frames: ConnectionFrame[];
}
