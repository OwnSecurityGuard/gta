/** get_agent_download_options 返回的单平台预置产物。 */
export interface AgentPlatform {
  /** 目标操作系统：windows / linux */
  os: string;
  /** 目标架构：amd64 / arm64 */
  arch: string;
  /** 展示名，如 "Windows x64" */
  label: string;
  /** 是否需要 .exe 后缀 */
  exe: boolean;
  /** 该平台产物是否已预置（false 时不可下载） */
  available: boolean;
  /** 磁盘文件名（含 .exe 时） */
  filename: string;
}

/** get_agent_download_options 返回：下载 Agent 页面需要的服务端信息 + 可下载平台矩阵（扁平结构 + ok）。 */
export interface GetAgentDownloadOptionsResult {
  ok: boolean;
  /** 本机可被远端 Agent 访问的地址（局域网 IP 或公网地址） */
  host: string;
  /** pipeline registry 监听地址，如 127.0.0.1:9091 */
  registry_addr: string;
  /** 推流 ingest 监听地址（registry 端口 +1） */
  ingest_addr: string;
  /** registry 端口（Agent 回连地址用） */
  registry_port: string;
  /** ingest 端口（Agent 自动推流用） */
  ingest_port: string;
  /** 可下载的目标平台矩阵（按预置产物存在与否标记可用性） */
  platforms: AgentPlatform[];
  /** 给用户的端口/地址说明文案 */
  message: string;
}