/** list_all_sessions 返回的单个 session 元数据 */
export interface SessionInfo {
  session_id: string;
  started_at: string;
  stopped_at: string;
  status: string;
  port: number;
  plugin: string;
  interface: string;
  pcap_file: string;
  raw_packets: number;
  events: number;
  metrics: number;
  decode_errors: number;
  duration_sec: number;
  db_path: string;
}

/** list_all_sessions 完整响应 */
export interface ListSessionsResult {
  ok: boolean;
  count: number;
  sessions: SessionInfo[];
}
