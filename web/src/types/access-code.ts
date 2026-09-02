/** 启动码 GTA-XXXX-XXXX：成员在目标机输入后自动注册设备并回连抓包。 */
export interface AccessCode {
  /** 启动码本体，形如 GTA-XXXX-XXXX */
  code: string;
  /** 创建者（owner）身份，匿名部署为空 */
  owner?: string;
  /** 绑定的项目 ID，可用于派生默认插件/端口 */
  project_id?: string;
  /** 绑定的解码插件 */
  plugin?: string;
  /** 抓包端口（Agent 自动生成 BPF 过滤） */
  port?: number;
  /** 覆盖回连地址（host:port），为空则用服务端部署地址 */
  server?: string;
  /** 目标平台，如 linux/amd64 */
  platform?: string;
  created_at: string;
  /** 过期时间（24h 有效） */
  expires_at: string;
  /** 是否已被认领（一次性） */
  claimed?: boolean;
  /** 认领后建立的服务端会话 */
  session_id?: string;
}

/** create_access_code 返回：新建的启动码。 */
export interface CreateAccessCodeResult {
  ok?: boolean;
  error?: string;
  code: string;
  project_id?: string;
  plugin?: string;
  port?: number;
  platform?: string;
  expires_at: string;
}

/** list_access_codes 返回：当前用户可见的启动码列表。 */
export interface ListAccessCodesResult {
  ok?: boolean;
  error?: string;
  codes: AccessCode[];
}