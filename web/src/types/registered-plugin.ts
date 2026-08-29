/** list_registered_plugins 返回的单个已注册插件摘要 */
export interface RegisteredPlugin {
  instance_id: string;
  name: string;
  protocol: string;
  type: string;
  api_version: string;
  socket_path: string;
  online: boolean;
  last_heartbeat: number;
  /** 注册者（团队模式下的用户名；匿名/系统插件为 local） */
  owner?: string;
}

/** list_registered_plugins 完整响应 */
export interface ListRegisteredPluginsResult {
  ok: boolean;
  plugins: RegisteredPlugin[];
}

/** set_session_plugin 响应 */
export interface SetSessionPluginResult {
  ok: boolean;
  session_id: string;
  plugin: string;
  message?: string;
}

/** deregister_plugin 响应 */
export interface DeregisterPluginResult {
  ok: boolean;
  instance_id: string;
  name: string;
}

/** start_capture 响应 */
export interface StartCaptureResult {
  ok: boolean;
  status: string;
  session_id: string;
  port: number;
  plugin: string;
  db_path: string;
  interface: string;
  /** nic | proxy */
  source: string;
  listen_addr: string;
}

/** stop_capture 响应 */
export interface StopCaptureResult {
  ok: boolean;
  status: string;
  session_id: string;
  raw_packets: number;
  events: number;
  metrics: number;
  decode_errors: number;
  duration_sec: number;
}
