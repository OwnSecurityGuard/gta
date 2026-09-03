/** 代理抓包租约快照（list_proxy_leases / get_proxy_lease / create_proxy_lease 返回）。
 * 每个租约 = 独立 mobile 抓包会话 + 独立 gta-singbox-agent 进程 + 私有筛选配置，
 * lease_id 与 session_id 一致（1:1 生命周期）。 */
export interface ProxyLease {
  /** 租约 ID（= session_id） */
  lease_id: string;
  /** 租约归属的 owner（多用户隔离） */
  owner: string;
  /** 租约会话归属的项目 id（可选） */
  project_id: string;
  /** 租约会话绑定的解码插件名（空=仅抓原始包不解码） */
  plugin: string;
  /** 连接筛选：仅抓取目标主机在此列表内的连接（空=不筛选） */
  include_hosts: string[];
  /** 连接筛选：仅抓取目标端口在此列表内的连接（空=不筛选） */
  include_ports: number[];
  /** 设备标签（如 alice-phone） */
  device: string;
  /** agent HTTP CONNECT 监听地址（手机连这里），如 0.0.0.0:12100 */
  listen_addr: string;
  /** agent 监听端口 */
  agent_listen_port: number;
  /** mobile Source gRPC 监听端口（agent 推送数据到这里） */
  mobile_grpc_port: number;
  /** gta-singbox-agent 子进程是否存活 */
  agent_running: boolean;
  /** agent 子进程 PID（未运行时为 0） */
  agent_pid: number;
  /** 租约会话是否运行中 */
  session_running: boolean;
  /** 租约会话 id（= lease_id） */
  session_id: string;
  /** 创建时间 unix 秒 */
  created_at_unix: number;
  /** 当前活跃手机连接数（open - close） */
  active_conns: number;
  /** 累计打开连接数 */
  total_conns: number;
  /** 最近一次收到数据的 unix 毫秒（0=从未收到数据） */
  last_data_unix: number;
  /** 累计接收应用层字节 */
  total_bytes: number;
  /** 本机局域网 IP（用于手机扫描二维码连接） */
  lan_ip: string;
  /** 手机代理软件填写的 HTTP CONNECT 代理地址（二维码内容），如 192.168.1.5:12100 */
  connect_addr: string;
  /** 手机 sing-box 客户端（SFA）可直接扫码导入的远程 profile URI，为空时前端回退到 connect_addr */
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

/** release_proxy_lease 完整响应。 */
export interface ReleaseProxyLeaseResult {
  ok: boolean;
  message: string;
  session_id: string;
}

/** 创建代理抓包租约的入参。 */
export interface CreateProxyLeaseVars {
  /** 解码插件名（空=仅抓原始包不解码） */
  plugin?: string;
  /** 连接筛选：仅抓取目标主机在此列表内的连接 */
  includeHosts?: string[];
  /** 连接筛选：仅抓取目标端口在此列表内的连接 */
  includePorts?: number[];
  /** 设备标签（用于识别，如 alice-phone） */
  device?: string;
  /** 租约会话归属的项目 id */
  projectId?: string;
}
