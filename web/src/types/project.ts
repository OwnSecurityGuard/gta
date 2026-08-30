/** 轻量「项目」模型：只保存 名称 + 默认解码插件 + 默认抓包端口，供一键开始抓包复用配置。 */
export interface ProjectInfo {
  id: string;
  name: string;
  owner?: string;
  /** 默认解码插件名（可为空） */
  plugin?: string;
  /** 默认抓包端口（0=未设置） */
  port?: number;
  created_at?: string;
  updated_at?: string;
}

/** list_projects 完整响应 */
export interface ListProjectsResult {
  ok?: boolean;
  projects: ProjectInfo[];
}

/** create/update_project 返回单个 project */
export interface ProjectResult {
  ok?: boolean;
  error?: string;
  project?: ProjectInfo;
  id?: string;
}