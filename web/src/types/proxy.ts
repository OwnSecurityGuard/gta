/** 代理抓包服务器配置 + 运行时状态（get_proxy_server_config 返回）。
 * 不含分帧配置：帧边界判定是协议语义，由绑定到会话的解码插件按连接自行处理。 */
export interface ProxyConfigState {
  /** agent HTTP CONNECT 代理监听地址（手机代理软件连这里），如 0.0.0.0:12000 */
  listen_addr: string;
  /** mobile Source gRPC 监听地址（agent 推送数据到这里），如 127.0.0.1:9090 */
  server_addr: string;
  /** gta-singbox-agent 子进程是否存活 */
  agent_running: boolean;
  /** agent 子进程 PID（未运行时为 0） */
  agent_pid: number;
  /** 常驻代理抓包会话是否运行中 */
  session_running: boolean;
  /** 常驻代理抓包会话 id */
  session_id: string;
  /** proxy.json 配置文件绝对路径 */
  config_path: string;
  /** 代理抓包会话绑定的解码插件名（空=仅抓原始包不解码） */
  plugin: string;
  /** 连接筛选：仅抓取目标主机在此列表内的连接（空=不筛选） */
  include_hosts: string[];
  /** 连接筛选：仅抓取目标端口在此列表内的连接（空=不筛选） */
  include_ports: number[];
  /** 当前活跃手机连接数（open - close），实时值来自常驻 mobile 会话 */
  active_conns: number;
  /** 累计打开连接数（当前常驻会话周期内） */
  total_conns: number;
  /** 最近一次收到数据的 unix 毫秒（0=从未收到数据） */
  last_data_unix: number;
  /** 累计接收应用层字节（当前常驻会话周期内） */
  total_bytes: number;
  /** 本机局域网 IP（用于手机扫描二维码连接） */
  lan_ip: string;
  /** 手机代理软件填写的 HTTP CONNECT 代理地址（二维码内容），如 192.168.1.5:12000 */
  connect_addr: string;
  /** 手机 sing-box 客户端（SFA）可直接扫码导入的远程 profile URI（sing-box://import-remote-profile?url=...#...），为空时前端回退到 connect_addr */
  singbox_uri: string;
}

/** get_proxy_server_config 完整响应。 */
export interface GetProxyConfigResult {
  ok: boolean;
  state: ProxyConfigState;
}

/** update_proxy_server_config 完整响应。 */
export interface UpdateProxyConfigResult {
  ok: boolean;
  message: string;
  state: ProxyConfigState;
}

/** 更新代理服务器配置的入参（空字段表示不修改）。
 * includeHosts/includePorts 传 undefined 表示不修改；传空数组表示清空筛选。 */
export interface ProxyConfigUpdateVars {
  listenAddr?: string;
  serverAddr?: string;
  /** 解码插件名（空=仅抓原始包不解码；分帧由插件自身实现） */
  plugin?: string;
  /** 连接筛选：仅抓取目标主机在此列表内的连接 */
  includeHosts?: string[];
  /** 连接筛选：仅抓取目标端口在此列表内的连接 */
  includePorts?: number[];
}
