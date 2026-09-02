/**
 * get_session_status 响应。成功路径有两种形态：
 * - gRPC 实时态：state/source_name/packets_in/raw_count/event_count/metric_count/decode_errors/drops/errors/err
 * - 元数据降级态：state/port/plugin/interface/pcap_file/raw_packets/events/metrics/decode_errors/duration_sec/db_path/manifest_snapshot?
 * 无会话时为 { state: "idle" } 或 { state: "closed", session_id }
 * 字段全部可选以兼容两种形态。
 */
export interface SessionStatusResult {
  ok?: boolean;
  session_id: string;
  state: string;
  source_name?: string;
  packets_in?: number;
  raw_count?: number;
  event_count?: number;
  metric_count?: number;
  decode_errors?: number;
  drops?: number;
  errors?: number;
  err?: string;
  port?: number;
  plugin?: string;
  interface?: string;
  pcap_file?: string;
  raw_packets?: number;
  events?: number;
  metrics?: number;
  duration_sec?: number;
  db_path?: string;
  manifest_snapshot?: string;
  /**
   * agent 源会话当前是否有活跃的推流连接。true 只代表「连上了」，与包数无关——
   * 「已连接但零流量」是极常见状态，UI 必须据此给出与「没连上」不同的指引。
   * 非 agent 源恒为 false。
   */
  agent_connected?: boolean;
  /** 最近一次收到该会话 agent 数据包的 Unix 秒；0/缺省 = 从未收到过数据。 */
  agent_last_seen_unix?: number;
}

/** list_interfaces 返回的单条网卡 */
export interface InterfaceInfo {
  name: string;
}

/** list_interfaces 完整响应 */
export interface ListInterfacesResult {
  ok?: boolean;
  interfaces: InterfaceInfo[];
}

/** delete_session 完整响应 */
export interface DeleteSessionResult {
  ok?: boolean;
  status: string;
  session_id: string;
}
