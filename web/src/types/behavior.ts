/** begin_capture_run 完整响应 */
export interface BeginCaptureRunResult {
  ok?: boolean;
  run_id: string;
  time_from: string;
  capture_status: string;
  capture_isolation_mode: string;
  session_id: string;
  uncertainties?: string[];
}

/** 行为窗口增量统计 */
export interface RunSummary {
  captured_flow_count: number;
  captured_message_count: number;
  client_request_count: number;
  server_message_count: number;
  decode_error_count: number;
}

/** end_capture_run 完整响应 */
export interface EndCaptureRunResult {
  ok?: boolean;
  run_id: string;
  time_to: string;
  duration_ms: number;
  summary: RunSummary;
  idempotent?: boolean;
}

/** get_run_status 完整响应 */
export interface RunStatusResult {
  ok?: boolean;
  run_id: string;
  status: string;
  time_from: string;
  flow_count?: number;
  client_request_count?: number;
  server_message_count?: number;
  decode_error_count?: number;
}

/** trace_protocol_flow：request/response 关键字段 */
export interface TraceKeyFields {
  [k: string]: unknown;
}
export interface TraceRequestSummary {
  name: string;
  direction: string;
  key_fields?: TraceKeyFields;
}
export interface TraceResponseSummary {
  msg_id: number;
  name: string;
  key_fields?: TraceKeyFields;
}
export interface TracePushSummary {
  msg_id: number;
  name: string;
  summary: string;
}
export interface TraceEntityDiff {
  uri: string;
  key: string;
  fields: string[];
}
export interface TraceStep {
  step_id: string;
  request_msg_id: number;
  request: TraceRequestSummary;
  response?: TraceResponseSummary;
  pushes?: TracePushSummary[];
  entity_diffs?: TraceEntityDiff[];
  why_related: string;
}
export interface TraceCloseInfo {
  closer: string;
  method: string;
  timestamp: string;
  msg_id: number;
  src: string;
  dst: string;
  note?: string;
}
export interface TraceTimeWindow {
  from: string;
  to: string;
}

/** trace_protocol_flow 完整响应（step_count/summary 仅在大结果写文件时返回） */
export interface TraceProtocolFlowResult {
  ok?: boolean;
  run_id: string;
  flow_id: string;
  feature_name: string;
  time_window: TraceTimeWindow;
  steps: TraceStep[];
  close_info?: TraceCloseInfo;
  uncertainties?: string[];
  file_path?: string;
  step_count?: number;
  summary?: unknown;
}
