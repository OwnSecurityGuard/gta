/** 是否启用「原始包」调试界面。
 * 对应后端 --enable-raw-debug / GTA_MCP_ENABLE_RAW_DEBUG=1。
 * 默认关闭，避免前端直接暴露原始包字节（满足"不暴露原始包"约束）。
 * 通过 .env 设置 VITE_ENABLE_RAW_DEBUG=1 开启（仅开发/插件调试场景）。 */
export const RAW_DEBUG_ENABLED =
  (import.meta.env as Record<string, string | boolean | undefined>).VITE_ENABLE_RAW_DEBUG ===
  "1";
