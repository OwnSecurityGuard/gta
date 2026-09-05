/** 探针相关类型（v2 探针优化，对应 MCP 工具 list_probes / get_probe 等）。 */

/** ProbeInfo：三维度状态快照（connection / capture / data）。 */
export interface ProbeInfo {
  probe_id: string;
  name: string;
  owner: string;
  tenant_id: string;
  capabilities: string; // csv: pcap,mobile,plugin_host
  version: string;
  hostname: string;
  os: string;
  arch: string;
  // 维度一：连接
  connection_state: "online" | "offline";
  last_seen_at: string; // RFC3339
  // 维度二：抓包状态机
  capture_state: "idle" | "starting" | "running" | "stopped" | "failed" | "";
  last_session_id: string;
  status_error: string;
  capture_iface: string;
  capture_ports: string; // csv
  // 维度三：数据
  last_packet_unix_ms: number;
  last_upload_unix_ms: number;
  packets_captured: number;
  packets_acked: number;
  spool_depth: number;
  dropped: number;
  // 归档摘要
  archive_bytes: number;
  archive_segments: number;
  archive_oldest_unix: number;
  archive_newest_unix: number;
  created_at: string;
}

export interface ListProbesResult {
  ok: boolean;
  probes: ProbeInfo[];
}

export interface GetProbeResult {
  ok: boolean;
  probe: ProbeInfo;
}

export interface ProbeStartCaptureResult {
  ok: boolean;
  session_id: string;
  probe_id: string;
}

export interface ProbeStopCaptureResult {
  ok: boolean;
  session_id: string;
}

export interface ProbeOkResult {
  ok: boolean;
}

export interface ProbeArchiveSegment {
  seg_id: string;
  first_unix: number;
  last_unix: number;
  packets: number;
  bytes: number;
  link_type: number;
}

export interface ProbeListArchiveResult {
  ok: boolean;
  segments: ProbeArchiveSegment[];
  /** true = 探针离线，结果来自服务端缓存（可能过期）。 */
  from_cache: boolean;
}

export interface ProbeImportArchiveResult {
  ok: boolean;
  session_id: string;
  probe_id: string;
}
