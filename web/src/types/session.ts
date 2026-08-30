/** list_all_sessions 返回的单个 session 元数据 */
export interface SessionInfo {
  /** 会话归属者（团队模式下的用户名；匿名/本地单机为空） */
  owner?: string;
  session_id: string;
  started_at: string;
  stopped_at: string;
  status: string;
  port: number;
  plugin: string;
  interface: string;
  pcap_file: string;
  /** 抓包来源 nic | proxy（代理抓包时存在） */
  source: string;
  listen_addr: string;
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
