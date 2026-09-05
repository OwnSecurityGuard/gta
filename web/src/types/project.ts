export type ProjectRole = "admin" | "member";

export interface ProjectMember {
  user: string;
  role: ProjectRole;
  /** 该用户名是否已有身份（users 表 / env bootstrap）。false = 预邀请，对方注册同名后生效。 */
  registered?: boolean;
}

export interface ProjectPlugin {
  id: string;
  name: string;
}

export interface ProjectRule {
  id: string;
  name: string;
}

/** 项目一等组织单元：持有 Game/Members/Decoder Plugins/Rules/Sessions。 */
export interface ProjectInfo {
  id: string;
  name: string;
  description?: string;
  game?: string;
  /** 创建者（审计字段，永不变更；不参与鉴权）。 */
  created_by?: string;
  /** 当前 Owner（SSOT，全权含删除项目与转移 Owner）。 */
  owner?: string;
  /** 归属租户（当前恒为 default）。 */
  tenant_id?: string;
  default_plugin?: string;
  default_port?: number;
  /** admin/member 成员表（Owner 不在此列，以 owner 字段为准）。 */
  members?: ProjectMember[];
  plugins?: ProjectPlugin[];
  rules?: ProjectRule[];
  created_at?: string;
  updated_at?: string;
}

export interface ProjectRecentSession {
  session_id: string;
  status: string;
  started_at: string;
  events?: number;
  raw_packets?: number;
}

export interface ProjectDetail extends ProjectInfo {
  recent_sessions?: ProjectRecentSession[];
}

/** list_projects 完整响应 */
export interface ListProjectsResult {
  ok?: boolean;
  projects: ProjectInfo[];
}

/** get_project 的返回：project 详情 + recent_sessions + capabilities */
export interface GetProjectResult {
  ok?: boolean;
  error?: string;
  project?: ProjectDetail;
  recent_sessions?: ProjectRecentSession[];
  /** 当前调用者被放行的管理动作（authz.Action 列表），前端据此渲染管理入口。 */
  capabilities?: string[];
}

/** authz.Action 子集：前端管理入口的渲染依据。 */
export type ProjectCapability =
  | "project:update"
  | "project:delete"
  | "project:manage_members"
  | "project:manage_plugins"
  | "project:manage_rules"
  | "project:transfer_owner";

/** create/update/delete_project 返回单个 project */
export interface ProjectResult {
  ok?: boolean;
  error?: string;
  project?: ProjectInfo;
  id?: string;
}