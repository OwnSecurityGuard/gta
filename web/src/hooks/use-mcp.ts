import { useEffect } from "react";
import { useQuery, useMutation, useQueryClient, keepPreviousData } from "@tanstack/react-query";
import { mcpClient } from "@/lib/mcp-client";
import type { ListSessionsResult } from "@/types/session";
import type { ListDecodedDataResult, CaptureSchemaResult } from "@/types/event";
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

/** 启动一次抓包会话（可指定解码插件） */
export function useStartCapture() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (vars: { port: number; plugin?: string; pcapFile?: string }) =>
      mcpClient.callTool<StartCaptureResult>("start_capture", {
        port: vars.port,
        plugin: vars.plugin ?? "",
        pcap_file: vars.pcapFile,
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
  useEffect(() => {
    const es = new EventSource("/events/plugins");
    es.addEventListener("plugin", () => {
      void queryClient.invalidateQueries({ queryKey: ["registeredPlugins"] });
      void queryClient.invalidateQueries({ queryKey: ["sessions"] });
    });
    es.onerror = () => {
      // EventSource 默认会自动重连；此处仅记录，无需手动处理。
      // 轮询兜底维持面板在重连间隙内的基本可用性。
      slogError("plugin event stream error, relying on polling fallback");
    };
    return () => es.close();
  }, [queryClient]);
}

// 轻量错误日志（避免引入额外依赖）。
function slogError(msg: string) {
  // eslint-disable-next-line no-console
  console.warn("[plugin-events]", msg);
}
