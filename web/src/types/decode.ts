/** list_plugins 返回的单个插件条目 */
export interface PluginInfo {
  name: string;
  binary: string;
  dir: string;
}

/** list_plugins 返回的完整响应（plugins 为插件对象数组，非字符串数组） */
export interface ListPluginsResult {
  ok: boolean;
  plugins: PluginInfo[];
  count?: number;
  warning?: string;
}

/** decode_raw_packets 返回的完整响应 */
export interface DecodeRawPacketsResult {
  ok: boolean;
  status: string;
  session_id: string;
  plugin: string;
  total_raw: number;
  decoded: number;
  decode_errors: number;
  clear_existing: boolean;
}
