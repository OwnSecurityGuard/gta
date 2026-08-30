/** get_agent_download_options 返回：下载 Agent 页面需要的服务端回连信息（扁平结构 + ok）。 */
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
  /** 服务端平台（os/arch），仅支持服务端本机平台抓包 */
  platform: string;
  /** 给用户的端口/地址说明文案 */
  message: string;
}