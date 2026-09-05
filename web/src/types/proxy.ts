/** 代理抓包租约快照（list_proxy_leases / get_proxy_lease / create_proxy_lease /
 *  start_lease_capture / stop_lease_capture 返回）。
 *
 * 租约（agent + 出口端口 + 控制端口）与抓包会话（mobile session + 抓到的
 *  SQLite）解耦：lease_id 跨多次抓包不变；session_id 在 idle 时为 ""，
 * 抓包中时是真实会话 id。旧版「lease_id = session_id 1:1」已废止。 */
export interface ProxyLease {
  /** 租约 ID（创建时分配，跨多次抓包不变）。 */
  lease_id: string;
  /** 租约归属的 owner（多用户隔离）。 */
  owner: string;
  /** 租约会话归属的项目 id（可选）。 */
  project_id: string;
  /** 租约绑定的解码插件名（空=仅抓原始包不解码）。 */
  plugin: string;
  /** 连接筛选：仅抓取目标主机在此列表内的连接（空=不筛选）。 */
  include_hosts: string[];
  /** 连接筛选：仅抓取目标端口在此列表内的连接（空=不筛选）。 */
  include_ports: number[];
  /** 设备标签（如 alice-phone）。 */
  device: string;
  /** agent HTTP CONNECT 监听地址（手机连这里），如 0.0.0.0:12100。 */
  listen_addr: string;
  /** agent 监听端口（租约内稳定，sticky 时跨租约复用）。 */
  agent_listen_port: number;
  /** agent 本地控制接口端口（pipeline 通过它切 start/stop）。 */
  control_port: number;
  /** mobile Source gRPC 监听端口（= 当前抓包会话占用；idle 时为 0）。 */
  mobile_grpc_port: number;
  /** gt-singbox-agent 子进程是否存活（与抓包开停无关，常驻）。 */
  agent_running: boolean;
  /** agent 子进程 PID（未运行时为 0）。 */
  agent_pid: number;
  /** 当前抓包会话是否运行中（= 抓包按钮显示的「抓包中」）。 */
  session_running: boolean;
  /** agent 是否正在推数据（CaptureGate 工作状态；idle 时 false）。 */
  capture_running: boolean;
  /** 当前抓包会话 id（idle 时为 ""）。 */
  session_id: string;
  /** 本租约累计开始过多少次抓包（含正在进行的）。 */
  capture_count: number;
  /** 最近一次 start/stop capture 的 unix 秒，0=从未。 */
  last_capture_at_unix: number;
  /** 创建时间 unix 秒。 */
  created_at_unix: number;
  /** 当前活跃手机连接数（open - close，仅抓包中非零）。 */
  active_conns: number;
  /** 累计打开连接数。 */
  total_conns: number;
  /** 最近一次收到数据的 unix 毫秒（0=从未）。 */
  last_data_unix: number;
  /** 累计接收应用层字节。 */
  total_bytes: number;
  /** 端口是否为 (owner,device) 复用端口（true 时同一设备再创建仍拿同一端口）。 */
  sticky_port: boolean;
  /** 本机局域网 IP（用于手机扫描二维码；docker / wsl 场景可被 -lan-ip 覆盖）。 */
  lan_ip: string;
  /** 手机代理软件填写的 HTTP CONNECT 代理地址（二维码内容）。 */
  connect_addr: string;
  /** 手机 sing-box 客户端（SFA）可直接扫码导入的远程 profile URI。 */
  singbox_uri: string;
}

/** list_proxy_leases 完整响应。 */
export interface ListProxyLeasesResult {
  ok: boolean;
  leases: ProxyLease[];
}

/** create_proxy_lease 完整响应。 */
export interface CreateProxyLeaseResult {
  ok: boolean;
  lease: ProxyLease;
}

/** get_proxy_lease 完整响应。 */
export interface GetProxyLeaseResult {
  ok: boolean;
  lease: ProxyLease;
}

/** release_proxy_lease 完整响应（= 杀 agent、收端口、删租约，幂等）。 */
export interface ReleaseProxyLeaseResult {
  ok: boolean;
  message: string;
  session_id: string;
}

/** start_lease_capture 完整响应（= 在已有租约上开新一轮抓包）。 */
export interface StartLeaseCaptureResult {
  ok: boolean;
  message: string;
  session_id: string;
  lease: ProxyLease;
}

/** stop_lease_capture 完整响应（= 停抓包回归 idle，租约/agent 保留）。 */
export interface StopLeaseCaptureResult {
  ok: boolean;
  message: string;
  session_id: string;
  raw_packets: number;
  events: number;
  duration_s: number;
}

/** 创建代理抓包租约的入参。 */
export interface CreateProxyLeaseVars {
  /** 解码插件名（空=仅抓原始包不解码）。 */
  plugin?: string;
  /** 连接筛选：仅抓取目标主机在此列表内的连接。 */
  includeHosts?: string[];
  /** 连接筛选：仅抓取目标端口在此列表内的连接。 */
  includePorts?: number[];
  /** 设备标签（用于识别，如 alice-phone）。 */
  device?: string;
  /** 租约会话归属的项目 id。 */
  projectId?: string;
  /** true=只建出口不立即抓包；默认 false 自动开抓包。 */
  noAutoStart?: boolean;
}

/** start_lease_capture 入参（在已有租约上开新一轮抓包）。 */
export interface StartLeaseCaptureVars {
  leaseId: string;
  /** 本次覆盖解码插件（空=沿用租约）。 */
  plugin?: string;
  /** 本次覆盖主机筛选。 */
  includeHosts?: string[];
  /** 本次覆盖端口筛选。 */
  includePorts?: number[];
}

/** stop_lease_capture 入参（停抓包回归 idle，租约保留）。 */
export interface StopLeaseCaptureVars {
  leaseId: string;
}
