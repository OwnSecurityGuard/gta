export type ProjectRole = "admin" | "member";

export interface ProjectMember {
  user: string;
  role: ProjectRole;
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
  created_by?: string;
  default_plugin?: string;
  default_port?: number;
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

/** get_project 的返回：project 详情 + recent_sessions */
export interface GetProjectResult {
  ok?: boolean;
  error?: string;
  project?: ProjectDetail;
  recent_sessions?: ProjectRecentSession[];
}

/** create/update/delete_project 返回单个 project */
export interface ProjectResult {
  ok?: boolean;
  error?: string;
  project?: ProjectInfo;
  id?: string;
}