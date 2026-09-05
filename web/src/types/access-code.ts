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
  /** 非空表示邀请码：认领时为该名字创建独立身份 */
  new_owner?: string;
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
  /** 邀请码标记：new_owner 非空时为 true */
  invite?: boolean;
  /** 邀请码目标身份名 */
  new_owner?: string;
}

/** list_access_codes 返回：当前用户可见的启动码列表。 */
export interface ListAccessCodesResult {
  ok?: boolean;
  error?: string;
  codes: AccessCode[];
}

/** 邀请制用户条目（list_users 返回；不含 token —— 凭证只在创建时展示一次）。 */
export interface GtaUser {
  owner: string;
  is_admin?: boolean;
  tenant_id?: string;
  created_by?: string;
  created_at: string;
}

/** list_users 返回：成员账号列表 + env bootstrap 身份（仅 global admin）。 */
export interface ListUsersResult {
  ok?: boolean;
  error?: string;
  users: GtaUser[];
  /** env bootstrap（GTA_AUTH_TOKENS）身份名：不在 users 表、不可撤销，仅展示。 */
  bootstrap_owners?: string[];
}

/** revoke_user 返回。 */
export interface RevokeUserResult {
  ok?: boolean;
  error?: string;
  owner: string;
  revoked?: boolean;
}