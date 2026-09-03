import { useEffect } from "react";
import { useQuery, useMutation, useQueryClient, keepPreviousData } from "@tanstack/react-query";
import { mcpClient } from "@/lib/mcp-client";
import { useAuthToken } from "@/hooks/use-auth";
import { withTokenParam, notifyAuthError, wasRecentlyUnauthorized } from "@/lib/auth";
import type { ListSessionsResult } from "@/types/session";
import type { ListDecodedDataResult, CaptureSchemaResult } from "@/types/event";
import type { SessionTimelineResult } from "@/types/timeline";
import type { ListRawPacketsResult } from "@/types/raw-packet";
import type { ListPluginsResult, DecodeRawPacketsResult } from "@/types/decode";
import type {
  ListRegisteredPluginsResult,
  SetSessionPluginResult,
  DeregisterPluginResult,
  StartCaptureResult,
  StopCaptureResult,
} from "@/types/registered-plugin";
import type { TestPluginResult, TestPluginVars } from "@/types/plugin-test";
import type { AggregateQueryResult } from "@/types/analytics";
import type {
  SessionStatusResult,
  ListInterfacesResult,
  DeleteSessionResult,
} from "@/types/session-extra";
import type {
  CreatePluginResult,
  BuildPluginResult,
  PluginStatusResult,
  ExplainPluginResult,
  PluginManifestResult,
} from "@/types/plugin-dev";
import type {
  BeginCaptureRunResult,
  EndCaptureRunResult,
  RunStatusResult,
  TraceProtocolFlowResult,
} from "@/types/behavior";
import type { QueryCaptureTableResult } from "@/types/table-browser";
import type { ListConnectionsResult, GetConnectionDetailResult, ListConnectionStreamsResult, ListConnectionFramesResult } from "@/types/connection";
import type {
  ListProxyLeasesResult,
  CreateProxyLeaseResult,
  ReleaseProxyLeaseResult,
  CreateProxyLeaseVars,
} from "@/types/proxy";
import type { GetAgentDownloadOptionsResult } from "@/types/agent";
import type { CreateAccessCodeResult, ListAccessCodesResult } from "@/types/access-code";
import type {
  ListProjectsResult,
  ProjectResult,
  ProjectRole,
  ProjectPlugin,
  ProjectRule,
  GetProjectResult,
} from "@/types/project";

/** 查询 session 列表 */
export function useSessions() {
  return useQuery({
    queryKey: ["sessions"],
    queryFn: () => mcpClient.callTool<ListSessionsResult>("list_all_sessions"),
    refetchInterval: 10_000, // 每 10s 自动刷新
  });
}

/** 查询指定 session 的解码数据 */
export function useDecodedData(
  sessionId: string | null,
  options: { limit?: number; offset?: number; filter?: string },
) {
  return useQuery({
    queryKey: ["decodedData", sessionId, options],
    queryFn: () =>
      mcpClient.callTool<ListDecodedDataResult>("list_decoded_data", {
        session_id: sessionId ?? undefined,
        limit: options.limit,
        offset: options.offset,
        filter: options.filter,
      }),
    enabled: !!sessionId,
    placeholderData: keepPreviousData, // 翻页/筛选时不闪骨架屏，沿用上一页数据
    // 抓包是实时写入，需要轮询才能把新解码的事件持续拉出来；
    // 没有轮询时查询只在 enabled 变 true 时触发一次，之后表格永远停留在那一刻的快照。
    refetchInterval: sessionId ? 2000 : false,
  });
}

/** 查询指定 session 的 schema 信息 */
export function useCaptureSchema(sessionId: string | null) {
  return useQuery({
    queryKey: ["schema", sessionId],
    queryFn: () =>
      mcpClient.callTool<CaptureSchemaResult>("get_capture_schema", {
        session_id: sessionId ?? undefined,
      }),
    enabled: !!sessionId,
    staleTime: 5 * 60_000, // schema 变化不频繁，缓存 5 分钟
  });
}

/** 查询指定 session 的原始包 */
export function useRawPackets(
  sessionId: string | null,
  options: { limit?: number; offset?: number; protocol?: string; src?: string; dst?: string },
) {
  return useQuery({
    queryKey: ["rawPackets", sessionId, options],
    queryFn: () =>
      mcpClient.callTool<ListRawPacketsResult>("list_raw_packets", {
        session_id: sessionId ?? undefined,
        limit: options.limit,
        offset: options.offset,
        protocol: options.protocol,
        src: options.src,
        dst: options.dst,
      }),
    enabled: !!sessionId,
    placeholderData: keepPreviousData, // 翻页时沿用上一页数据，避免骨架屏闪烁
    refetchInterval: sessionId ? 2000 : false,
  });
}

/** 列出可用解码插件 */
export function useListPlugins() {
  return useQuery({
    queryKey: ["plugins"],
    queryFn: () => mcpClient.callTool<ListPluginsResult>("list_plugins"),
    staleTime: 60_000, // 插件列表变化不频繁，缓存 1 分钟
  });
}

/**
 * 用插件解码离线会话的原始包。
 * 成功后失效该 session 的 decodedData 与 rawPackets 缓存，
 * 调用方应提示用户切换到"协议数据"Tab 查看解码结果。
 */
export function useDecodeRawPackets() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (vars: {
      sessionId: string;
      plugin: string;
      protocol?: string;
      src?: string;
      dst?: string;
      limit?: number;
      clearExisting?: boolean;
    }) =>
      mcpClient.callTool<DecodeRawPacketsResult>("decode_raw_packets", {
        session_id: vars.sessionId,
        plugin: vars.plugin,
        protocol: vars.protocol,
        src: vars.src,
        dst: vars.dst,
        limit: vars.limit,
        clear_existing: vars.clearExisting ?? true,
      }),
    onSuccess: (_data, vars) => {
      // 解码结果写入 decoded_events，需失效该 session 的解码数据缓存
      void queryClient.invalidateQueries({ queryKey: ["decodedData", vars.sessionId] });
      // rawPackets 计数虽不变，但 invalidate 以保持一致性
      void queryClient.invalidateQueries({ queryKey: ["rawPackets", vars.sessionId] });
    },
  });
}

/** 用指定插件对离线会话原始包解码并采样返回（隐私安全：不回传原始包、不落库）。 */
export function useTestPlugin() {
  return useMutation({
    mutationFn: (vars: TestPluginVars) =>
      mcpClient.callTool<TestPluginResult>("test_plugin", {
        session_id: vars.sessionId,
        plugin: vars.plugin,
        protocol: vars.protocol ?? "",
        src: vars.src ?? "",
        dst: vars.dst ?? "",
        limit: vars.limit ?? 0,
        sample_limit: vars.sampleLimit ?? 50,
      }),
  });
}

/** 列出已注册（在线/离线）的解码插件，含 instance_id 与心跳，用于热更可视化 */
export function useRegisteredPlugins() {
  return useQuery({
    queryKey: ["registeredPlugins"],
    queryFn: () => mcpClient.callTool<ListRegisteredPluginsResult>("list_registered_plugins"),
    refetchInterval: 5_000, // 每 5s 轮询，使热更（instance_id 变化）快速可见
  });
}

/**
 * 运行中热切换某抓包会话绑定的解码插件。
 * 成功后失效 sessions 与 registeredPlugins 缓存，使侧边栏与插件面板同步。
 */
export function useSetSessionPlugin() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (vars: { sessionId: string; plugin: string }) =>
      mcpClient.callTool<SetSessionPluginResult>("set_session_plugin", {
        session_id: vars.sessionId,
        plugin: vars.plugin,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["sessions"] });
      void queryClient.invalidateQueries({ queryKey: ["registeredPlugins"] });
    },
  });
}

/** 强制下线某个注册插件（按 instance_id 或 name），用于崩溃插件清理 */
export function useDeregisterPlugin() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (vars: { instanceId?: string; name?: string }) =>
      mcpClient.callTool<DeregisterPluginResult>("deregister_plugin", {
        instance_id: vars.instanceId,
        name: vars.name,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["registeredPlugins"] });
    },
  });
}

/** 启动一次抓包会话（可指定来源与解码插件）。 */
export function useStartCapture() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (vars: {
      port: number;
      plugin?: string;
      pcapFile?: string;
      /** nic | proxy */
      source?: string;
      listenAddr?: string;
      /** 从项目一键抓包时绑定的项目 id，抓包会话归属到该项目 */
      projectId?: string;
    }) =>
      mcpClient.callTool<StartCaptureResult>("start_capture", {
        port: vars.port,
        plugin: vars.plugin ?? "",
        pcap_file: vars.pcapFile,
        source: vars.source ?? "nic",
        project_id: vars.projectId ?? "",
        listen_addr: vars.source === "proxy" ? (vars.listenAddr ?? "127.0.0.1:9090") : undefined,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["sessions"] });
    },
  });
}

/** 停止指定 session 的抓包 */
export function useStopCapture() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (vars: { sessionId: string }) =>
      mcpClient.callTool<StopCaptureResult>("stop_capture", {
        session_id: vars.sessionId,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["sessions"] });
    },
  });
}

/**
 * 订阅后端插件事件 SSE 流（/events/plugins），收到 register/deregister/online/offline
 * 事件即失效 registeredPlugins 与 sessions 查询，使前端在插件上下线/热更时零延迟刷新。
 * 5s 轮询保留作断线兜底；EventSource 默认断线自动重连。应在应用顶层挂载一次。
 */
export function usePluginEventStream() {
  const queryClient = useQueryClient();
  // token 变化时重建连接：EventSource 无法中途补头，也读不到新 token。
  const token = useAuthToken();
  useEffect(() => {
    const es = new EventSource(withTokenParam("/events/plugins"));
    es.addEventListener("plugin", () => {
      void queryClient.invalidateQueries({ queryKey: ["registeredPlugins"] });
      void queryClient.invalidateQueries({ queryKey: ["sessions"] });
    });
    es.onerror = () => {
      // EventSource 默认会自动重连；此处仅记录，无需手动处理。
      // 轮询兜底维持面板在重连间隙内的基本可用性。
      if (es.readyState === EventSource.CLOSED) {
        // CLOSED 不代表一定是 401（代理 5xx/后端重启同样进入 CLOSED 终态）。
        // 仅当 mcp-client 最近上报过 401 才置位横幅；token 模式下轮询必然
        // 在 10s 窗口内产生 401 信号，真实 401 不会漏报。
        if (wasRecentlyUnauthorized()) {
          notifyAuthError();
        } else {
          slogError("plugin event stream closed (non-401), relying on polling fallback");
        }
        return;
      }
      slogError("plugin event stream error, relying on polling fallback");
    };
    return () => es.close();
  }, [queryClient, token]);
}

// ===== 分析（聚合统计）=====

/** aggregate_query：用 expr 表达式查询预计算聚合指标。 */
export function useAggregateQuery(expression: string, sessionId: string | null) {
  const expr = expression.trim();
  return useQuery({
    queryKey: ["aggregate", sessionId, expr],
    queryFn: () =>
      mcpClient.callTool<AggregateQueryResult>("aggregate_query", {
        expression: expr,
        session_id: sessionId ?? undefined,
      }),
    enabled: !!sessionId && expr.length > 0,
    staleTime: 30_000,
  });
}

// ===== 会话时间线（执行链 / 请求-响应因果树）=====

/** get_session_timeline：构建整 session 的 request/response 因果树（TraceContext 执行链）。 */
export function useSessionTimeline(
  sessionId: string | null,
  options: { limit?: number; offset?: number },
) {
  const { limit, offset } = options;
  return useQuery({
    queryKey: ["sessionTimeline", sessionId, options],
    queryFn: () =>
      mcpClient.callTool<SessionTimelineResult>("get_session_timeline", {
        session_id: sessionId ?? undefined,
        limit: limit ?? 500,
        offset: offset ?? 0,
      }),
    enabled: !!sessionId,
    staleTime: 15_000,
    refetchInterval: sessionId ? 5000 : false,
  });
}

// ===== 会话增强（状态 / 删除 / 网卡）=====

/** get_session_status：查询指定会话的实时状态（gRPC 优先，失败降级元数据）。 */
export function useSessionStatus(sessionId: string | null, refetchInterval = 5000) {
  return useQuery({
    queryKey: ["sessionStatus", sessionId],
    queryFn: () =>
      mcpClient.callTool<SessionStatusResult>("get_session_status", {
        session_id: sessionId ?? undefined,
      }),
    enabled: !!sessionId,
    refetchInterval,
  });
}

/** list_interfaces：列出可用抓包网卡。 */
export function useListInterfaces() {
  return useQuery({
    queryKey: ["interfaces"],
    queryFn: () => mcpClient.callTool<ListInterfacesResult>("list_interfaces"),
    staleTime: 5 * 60_000,
  });
}

/** delete_session：删除一个会话及其数据（破坏性，调用方需二次确认）。 */
export function useDeleteSession() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (vars: { sessionId: string }) =>
      mcpClient.callTool<DeleteSessionResult>("delete_session", { session_id: vars.sessionId }),
    onSuccess: (_data, vars) => {
      void queryClient.invalidateQueries({ queryKey: ["sessions"] });
      void queryClient.invalidateQueries({ queryKey: ["sessionStatus", vars.sessionId] });
    },
  });
}

/** 批量删除多个会话（逐个调用 delete_session，统计失败数）。 */
export function useDeleteSessions() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (sessionIds: string[]) => {
      const results = await Promise.allSettled(
        sessionIds.map((id) =>
          mcpClient.callTool<DeleteSessionResult>("delete_session", { session_id: id }),
        ),
      );
      const failed = results.filter(
        (r): r is PromiseRejectedResult => r.status === "rejected",
      ).length;
      return { total: sessionIds.length, failed };
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["sessions"] });
      void queryClient.invalidateQueries({ queryKey: ["sessionStatus"] });
    },
  });
}

// ===== 轻量「项目」模型（名称 + 默认插件 + 默认端口）=====

/** list_projects：列出当前可见项目。 */
export function useProjects() {
  return useQuery({
    queryKey: ["projects"],
    queryFn: () => mcpClient.callTool<ListProjectsResult>("list_projects"),
    staleTime: 10_000,
  });
}

/** create_project：新建项目。 */
export function useCreateProject() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (vars: { name: string; plugin?: string; port?: number }) =>
      mcpClient.callTool<ProjectResult>("create_project", {
        name: vars.name,
        plugin: vars.plugin ?? "",
        port: vars.port ?? 0,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["projects"] });
    },
  });
}

/** update_project：更新项目。 */
export function useUpdateProject() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (vars: { id: string; name?: string; plugin?: string; port?: number }) =>
      mcpClient.callTool<ProjectResult>("update_project", {
        id: vars.id,
        name: vars.name ?? "",
        plugin: vars.plugin ?? "",
        port: vars.port ?? 0,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["projects"] });
    },
  });
}

/** delete_project：删除项目。 */
export function useDeleteProject() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (vars: { id: string }) =>
      mcpClient.callTool<ProjectResult>("delete_project", { id: vars.id }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["projects"] });
    },
  });
}

/** get_project：查询单个项目的详情与最近会话。 */
export function useProject(id?: string) {
  return useQuery({
    queryKey: ["project", id],
    queryFn: () =>
      id
        ? mcpClient.callTool<GetProjectResult>("get_project", { id })
        : Promise.resolve(null),
    enabled: !!id,
  });
}

/** set_session_project：把会话绑定到某个项目。成功后失效 sessions 缓存。 */
export function useSetSessionProject() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { session_id: string; project_id?: string }) =>
      mcpClient.callTool<{ ok?: boolean }>("set_session_project", v),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["sessions"] }),
  });
}

/** add_project_member：向项目添加成员。 */
export function useAddProjectMember(projectId?: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { user: string; role: ProjectRole }) =>
      mcpClient.callTool<{ ok?: boolean }>("add_project_member", {
        project_id: projectId, ...v,
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["project", projectId] }),
  });
}

/** remove_project_member：从项目移除成员。 */
export function useRemoveProjectMember(projectId?: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { user: string }) =>
      mcpClient.callTool<{ ok?: boolean }>("remove_project_member", {
        project_id: projectId, ...v,
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["project", projectId] }),
  });
}

/** set_project_plugins：设置项目的解码插件集合。 */
export function useSetProjectPlugins(projectId?: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { plugins: ProjectPlugin[] }) =>
      mcpClient.callTool<{ ok?: boolean }>("set_project_plugins", {
        project_id: projectId,
        plugins: JSON.stringify(v.plugins),
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["project", projectId] }),
  });
}

/** set_project_rules：设置项目的解析规则集合。 */
export function useSetProjectRules(projectId?: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { rules: ProjectRule[] }) =>
      mcpClient.callTool<{ ok?: boolean }>("set_project_rules", {
        project_id: projectId,
        rules: JSON.stringify(v.rules),
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["project", projectId] }),
  });
}

// ===== 插件开发平面（脚手架 / 编译 / 状态 / 归因 / manifest）=====

/** create_plugin：从模板脚手架一个新解码插件工程。 */
export function useCreatePlugin() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (vars: {
      name: string;
      protocol: string;
      protocolVersion?: string;
      hints?: string[];
      outputDir?: string;
    }) =>
      mcpClient.callTool<CreatePluginResult>("create_plugin", {
        name: vars.name,
        protocol: vars.protocol,
        protocol_version: vars.protocolVersion ?? "",
        hints: vars.hints && vars.hints.length > 0 ? JSON.stringify(vars.hints) : "",
        output_dir: vars.outputDir ?? "",
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["plugins"] });
    },
  });
}

/** build_plugin：编译脚手架插件，返回结构化诊断。 */
export function useBuildPlugin() {
  return useMutation({
    mutationFn: (vars: { name: string; timeoutSec?: number }) =>
      mcpClient.callTool<BuildPluginResult>("build_plugin", {
        name: vars.name,
        timeout_sec: vars.timeoutSec ?? 120,
      }),
  });
}

/** status_plugin：查询插件双态视图（制品态 + 运行时态）。 */
export function usePluginStatus(name: string | null) {
  return useQuery({
    queryKey: ["pluginStatus", name],
    queryFn: () => mcpClient.callTool<PluginStatusResult>("status_plugin", { name: name! }),
    enabled: !!name,
    refetchInterval: name ? 5000 : false,
  });
}

/** explain_plugin：归因插件最近一次构建/激活失败。 */
export function useExplainPlugin() {
  return useMutation({
    mutationFn: (vars: { name: string; action?: string }) =>
      mcpClient.callTool<ExplainPluginResult>("explain_plugin", {
        name: vars.name,
        action: vars.action ?? "",
      }),
  });
}

/** get_plugin_manifest：返回已注册插件的 plugin.yaml 原文。 */
export function usePluginManifest(name: string | null) {
  return useQuery({
    queryKey: ["pluginManifest", name],
    queryFn: () => mcpClient.callTool<PluginManifestResult>("get_plugin_manifest", { name: name! }),
    enabled: !!name,
  });
}

// ===== 行为 / 因果（Runs）=====

/** begin_capture_run：标记一次用户操作窗口的开始。 */
export function useBeginCaptureRun() {
  return useMutation({
    mutationFn: (vars: {
      featureName: string;
      projectPath: string;
      pluginName?: string;
      device?: string;
      filter?: string;
      port?: number;
    }) =>
      mcpClient.callTool<BeginCaptureRunResult>("begin_capture_run", {
        feature_name: vars.featureName,
        project_path: vars.projectPath,
        plugin_name: vars.pluginName ?? "",
        device: vars.device ?? "",
        filter: vars.filter ?? "",
        port: vars.port ?? 0,
      }),
  });
}

/** end_capture_run：关闭操作窗口，返回窗口内增量统计（幂等）。 */
export function useEndCaptureRun() {
  return useMutation({
    mutationFn: (vars: { runId: string; timeTo?: string }) =>
      mcpClient.callTool<EndCaptureRunResult>("end_capture_run", {
        run_id: vars.runId,
        time_to: vars.timeTo ?? "",
      }),
  });
}

/** get_run_status：快速检查某 run 是否有用数据。 */
export function useRunStatus(runId: string | null) {
  return useQuery({
    queryKey: ["runStatus", runId],
    queryFn: () => mcpClient.callTool<RunStatusResult>("get_run_status", { run_id: runId! }),
    enabled: !!runId,
    refetchInterval: runId ? 3000 : false,
  });
}

/** trace_protocol_flow：构建一次行为的时序执行链路。 */
export function useTraceProtocolFlow() {
  return useMutation({
    mutationFn: (vars: { runId: string; flowId: string; featureName: string }) =>
      mcpClient.callTool<TraceProtocolFlowResult>("trace_protocol_flow", {
        run_id: vars.runId,
        flow_id: vars.flowId,
        feature_name: vars.featureName,
      }),
  });
}

// ===== 表浏览器（只读逃生口）=====

/** query_capture_table：只读查询内部投影/审计表。 */
export function useQueryCaptureTable(
  sessionId: string | null,
  table: string,
  options: { limit?: number; offset?: number },
) {
  return useQuery({
    queryKey: ["captureTable", sessionId, table, options],
    queryFn: () =>
      mcpClient.callTool<QueryCaptureTableResult>("query_capture_table", {
        session_id: sessionId ?? undefined,
        table,
        limit: options.limit ?? 100,
        offset: options.offset ?? 0,
      }),
    enabled: !!sessionId && table.length > 0,
    staleTime: 15_000,
  });
}

// 轻量错误日志（避免引入额外依赖）。
function slogError(msg: string) {
  // eslint-disable-next-line no-console
  console.warn("[plugin-events]", msg);
}

// ===== 代理抓包连接（Connections 页面）=====

/** list_connections：按 conn_id 聚合返回代理抓包连接列表（最新在前）。 */
export function useConnections(
  sessionId: string | null,
  options: { limit?: number; offset?: number },
) {
  return useQuery({
    queryKey: ["connections", sessionId, options],
    queryFn: () =>
      mcpClient.callTool<ListConnectionsResult>("list_connections", {
        session_id: sessionId ?? undefined,
        limit: options.limit ?? 100,
        offset: options.offset ?? 0,
      }),
    enabled: !!sessionId,
    placeholderData: keepPreviousData,
    refetchInterval: sessionId ? 2000 : false, // 抓包实时写入，轮询保持列表更新
  });
}

/** get_connection_detail：查询单个连接的详情（头部 + 统计）。 */
export function useConnectionDetail(sessionId: string | null, connId: string | null) {
  return useQuery({
    queryKey: ["connectionDetail", sessionId, connId],
    queryFn: () =>
      mcpClient.callTool<GetConnectionDetailResult>("get_connection_detail", {
        session_id: sessionId ?? undefined,
        conn_id: connId!,
      }),
    enabled: !!sessionId && !!connId,
    refetchInterval: sessionId && connId ? 2000 : false,
  });
}

/** list_connection_streams：查询连接内的流（Stream View）。 */
export function useConnectionStreams(
  sessionId: string | null,
  connId: string | null,
  options: { limit?: number; offset?: number },
) {
  return useQuery({
    queryKey: ["connectionStreams", sessionId, connId, options],
    queryFn: () =>
      mcpClient.callTool<ListConnectionStreamsResult>("list_connection_streams", {
        session_id: sessionId ?? undefined,
        conn_id: connId!,
        limit: options.limit ?? 200,
        offset: options.offset ?? 0,
      }),
    enabled: !!sessionId && !!connId,
    placeholderData: keepPreviousData,
    refetchInterval: sessionId && connId ? 2000 : false,
  });
}

/** list_connection_frames：查询连接内的原始帧（Frames / Raw）。 */
export function useConnectionFrames(
  sessionId: string | null,
  connId: string | null,
  options: { limit?: number; offset?: number },
) {
  return useQuery({
    queryKey: ["connectionFrames", sessionId, connId, options],
    queryFn: () =>
      mcpClient.callTool<ListConnectionFramesResult>("list_connection_frames", {
        session_id: sessionId ?? undefined,
        conn_id: connId!,
        limit: options.limit ?? 100,
        offset: options.offset ?? 0,
      }),
    enabled: !!sessionId && !!connId,
    placeholderData: keepPreviousData,
    refetchInterval: sessionId && connId ? 2000 : false,
  });
}

// ===== 远程 Agent 下载 =====

/** get_agent_download_options：返回下载 Agent 页面需要的服务端信息（可达 IP / registry+ingest 端口 / 平台）。 */
export function useAgentDownloadOptions() {
  return useQuery({
    queryKey: ["agentDownloadOptions"],
    queryFn: () => mcpClient.callTool<GetAgentDownloadOptionsResult>("get_agent_download_options"),
    staleTime: 30_000, // 端口/平台变化不频繁，缓存 30s
  });
}

// ===== 代理抓包租约（按用户/设备独立会话，多用户互不串流）=====

/** list_proxy_leases：查询当前用户可见的代理抓包租约列表（admin 全可见）。 */
export function useProxyLeases() {
  return useQuery({
    queryKey: ["proxyLeases"],
    queryFn: () => mcpClient.callTool<ListProxyLeasesResult>("list_proxy_leases"),
    refetchInterval: 4000, // agent/会话/连接状态实时变化，轮询保持新鲜
  });
}

/** create_proxy_lease：创建独立代理抓包租约（独立端口 + 独立 agent + 独立会话）。 */
export function useCreateProxyLease() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (vars: CreateProxyLeaseVars) =>
      mcpClient.callTool<CreateProxyLeaseResult>("create_proxy_lease", {
        plugin: vars.plugin ?? "",
        include_hosts: vars.includeHosts ?? [],
        include_ports: vars.includePorts ?? [],
        device: vars.device ?? "",
        project_id: vars.projectId ?? "",
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["proxyLeases"] });
      void queryClient.invalidateQueries({ queryKey: ["sessions"] });
    },
  });
}

/** release_proxy_lease：释放租约（停会话 + 杀 agent + 回收端口，幂等）。 */
export function useReleaseProxyLease() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (leaseId: string) =>
      mcpClient.callTool<ReleaseProxyLeaseResult>("release_proxy_lease", { lease_id: leaseId }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["proxyLeases"] });
      void queryClient.invalidateQueries({ queryKey: ["sessions"] });
    },
  });
}

// ===== 启动码接入（GTA-XXXX，成员目标机免参数回连抓包）=====

/** list_access_codes：列出当前用户可见的启动码。 */
export function useAccessCodes() {
  return useQuery({
    queryKey: ["accessCodes"],
    queryFn: () => mcpClient.callTool<ListAccessCodesResult>("list_access_codes"),
    staleTime: 10_000,
    // 「我的设备」接入闭环靠轮询驱动：认领/会话状态变化需在数秒内反映到页面。
    refetchInterval: 4_000,
  });
}

/** create_access_code：生成一个绑定当前用户的启动码（可选绑项目/插件/端口/平台/回连地址）。 */
export function useCreateAccessCode() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (vars: {
      projectId?: string;
      plugin?: string;
      port?: number;
      platform?: string;
      server?: string;
    }) =>
      mcpClient.callTool<CreateAccessCodeResult>("create_access_code", {
        project_id: vars.projectId ?? "",
        plugin: vars.plugin ?? "",
        port: vars.port ?? 0,
        platform: vars.platform ?? "",
        server: vars.server ?? "",
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["accessCodes"] });
    },
  });
}
