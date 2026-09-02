/** create_plugin 完整响应 */
export interface CreatePluginResult {
  ok?: boolean;
  name: string;
  output_dir: string;
  created: string[];
  sdk_version: string;
  framing_available: boolean;
}

/** build_plugin 的结构化诊断（file:line:col） */
export interface BuildError {
  file: string;
  line: number;
  col: number;
  message: string;
}

/**
 * build_plugin 完整响应。
 * 注意：服务端 successResult 会强制把顶层 `ok` 重写为 true，因此构建是否成功
 * 不能以 `ok` 判定，而应以 errors 数组是否为空为准（前端计算 buildOk）。
 */
export interface BuildPluginResult {
  ok: boolean;
  name: string;
  output: string;
  errors: BuildError[];
}

/** status_plugin：制品态（Developer Plane，来自磁盘） */
export interface PluginStatusArtifact {
  state: string;
  source_dir?: string;
  binary_path?: string;
  binary_stale: boolean;
}

/** status_plugin：运行时态（Runtime Plane，来自注册表） */
export interface PluginStatusRuntime {
  state: string;
  instance_id?: string;
  online: boolean;
  last_heartbeat: number;
  bound_sessions?: unknown[];
}

/** status_plugin：开发进程态 */
export interface PluginStatusDevProcess {
  launched: boolean;
  pid?: number;
  instance_id?: string;
  alive?: boolean;
  launched_at?: number;
}

/** status_plugin：最近一次尝试的诊断 */
export interface PluginStatusAttemptError {
  file: string;
  line: number;
  col: number;
  message: string;
}
export interface PluginStatusLastAttempt {
  action?: string;
  ok?: boolean;
  at_unix?: number;
  duration_ms?: number;
  message?: string;
  explain_ref?: string;
  errors?: PluginStatusAttemptError[];
}

/** status_plugin：建议下一步 */
export interface PluginStatusNextAction {
  tool?: string;
  why?: string;
}

/** status_plugin 完整响应 */
export interface PluginStatusResult {
  ok?: boolean;
  name: string;
  artifact: PluginStatusArtifact;
  runtime: PluginStatusRuntime;
  dev_process: PluginStatusDevProcess;
  last_attempt: PluginStatusLastAttempt;
  next_action: PluginStatusNextAction | null;
}

/** explain_plugin：单条发现（根因） */
export interface ExplainFindingError {
  file: string;
  line: number;
  col: number;
  message: string;
}
export interface ExplainFinding {
  category: string;
  rule_id: string;
  why: string;
  fix: string;
  error?: ExplainFindingError;
}

/** explain_plugin 完整响应 */
export interface ExplainPluginResult {
  ok?: boolean;
  ref?: string;
  name: string;
  action?: string;
  at_unix?: number;
  summary?: string;
  next_action?: string;
  findings: ExplainFinding[];
}

/** get_plugin_manifest 完整响应 */
export interface PluginManifestResult {
  ok?: boolean;
  name: string;
  manifest: string;
}
