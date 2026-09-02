import { useState, useEffect } from "react";
import {
  useBeginCaptureRun,
  useEndCaptureRun,
  useRunStatus,
  useTraceProtocolFlow,
} from "@/hooks/use-mcp";
import type { BeginCaptureRunResult, RunStatusResult, TraceProtocolFlowResult } from "@/types/behavior";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import { Button } from "@/components/ui/button";
import { toast } from "@/components/ui/toast";
import { Flag, Square, Activity, GitBranch, Info, Link2 } from "lucide-react";

const inputCls =
  "h-8 rounded-md border border-input bg-background px-2 text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30";

/** 状态徽标颜色 */
function statusTone(status: string): string {
  if (status === "open" || status === "running") return "border-success/30 bg-success/10 text-success";
  if (status === "closed" || status === "complete") return "border-muted bg-muted text-muted-foreground";
  return "border-amber-300 bg-amber-50 text-amber-700";
}

function StatRow({ label, value }: { label: string; value: number | undefined }) {
  return (
    <div className="flex items-center justify-between text-xs">
      <span className="text-muted-foreground">{label}</span>
      <span className="tabular-nums">{value ?? 0}</span>
    </div>
  );
}

export function RunsPanel({
  linkedRunId = null,
  linkedSessionId = null,
  tracePrefill = null,
}: {
  /** 由 start_capture 自动开启的行为窗口 run_id */
  linkedRunId?: string | null;
  /** 该窗口联动的抓包会话 session_id */
  linkedSessionId?: string | null;
  /** 从连接详情「flow_id 一键跳转」预填的 flow_id */
  tracePrefill?: string | null;
}) {
  // 开始窗口表单
  const [featureName, setFeatureName] = useState("");
  const [projectPath, setProjectPath] = useState("");
  const [pluginName, setPluginName] = useState("");
  const [device, setDevice] = useState("");
  const [filter, setFilter] = useState("");
  const [port, setPort] = useState("");

  // 当前窗口
  const [runId, setRunId] = useState<string | null>(null);
  const [beginResult, setBeginResult] = useState<BeginCaptureRunResult | null>(null);

  // 行为链追踪
  const [flowId, setFlowId] = useState("");
  const [traceFeature, setTraceFeature] = useState("");
  const [traceResult, setTraceResult] = useState<TraceProtocolFlowResult | null>(null);

  const begin = useBeginCaptureRun();
  const end = useEndCaptureRun();
  const status = useRunStatus(runId);
  const trace = useTraceProtocolFlow();

  // 抓包会话自动开启的窗口：同步到当前 runId，使状态/追踪区块立即可用。
  useEffect(() => {
    if (linkedRunId && linkedRunId !== runId) setRunId(linkedRunId);
  }, [linkedRunId, runId]);

  // 从连接详情跳转过来的 flow_id：预填到「构建时序链路」输入框。
  useEffect(() => {
    if (tracePrefill) setFlowId(tracePrefill);
  }, [tracePrefill]);

  function handleBegin() {
    if (!featureName || !projectPath) return;
    setBeginResult(null);
    begin.mutate(
      {
        featureName,
        projectPath,
        pluginName: pluginName || undefined,
        device: device || undefined,
        filter: filter || undefined,
        port: port ? Number(port) : undefined,
      },
      {
        onSuccess: (res) => {
          setBeginResult(res ?? null);
          setRunId(res?.run_id ?? null);
          setTraceFeature(featureName);
          toast.success("已开启行为窗口", res?.run_id ?? "");
        },
        onError: (err) => toast.error("开启窗口失败", err.message),
      },
    );
  }

  function handleEnd() {
    if (!runId) return;
    end.mutate(
      { runId },
      {
        onSuccess: (res) => {
          toast.success(
            "窗口已关闭",
            `流 ${res?.summary?.captured_flow_count ?? 0} · 消息 ${res?.summary?.captured_message_count ?? 0}`,
          );
        },
        onError: (err) => toast.error("关闭窗口失败", err.message),
      },
    );
  }

  function handleTrace() {
    if (!runId || !flowId || !traceFeature) return;
    setTraceResult(null);
    trace.mutate(
      { runId, flowId, featureName: traceFeature },
      {
        onSuccess: (res) => {
          setTraceResult(res ?? null);
          if (res?.file_path) toast.success("行为链已写出文件", res.file_path);
        },
        onError: (err) => toast.error("构建行为链失败", err.message),
      },
    );
  }

  const statusData = status.data as RunStatusResult | undefined;

  return (
    <div className="h-full overflow-auto p-4 gta-scroll space-y-5">
      <EmptyState
        icon={<Flag className="h-5 w-5" />}
        title="行为窗口（Runs）"
        hint="标记一次用户操作窗口，随后用 begin/end/status/trace_protocol_flow 关联抓包数据、构建时序链路。注意：本工具只记录窗口本身，不会自动开始抓包；但通过「开始抓包」启动会话时，系统会自动开启一个联动窗口（reuse_existing 模式复用同一 session），无需手动 begin。"
      />

      {/* 联动提示：由「开始抓包」自动开启的窗口 */}
      {linkedSessionId && (
        <div className="flex items-center gap-2 rounded-md border border-primary/30 bg-primary-muted px-3 py-2 text-xs text-accent-foreground">
          <Link2 className="h-4 w-4 shrink-0" />
          <span>
            已与抓包会话 <span className="font-mono">{linkedSessionId}</span> 联动
            {linkedRunId && <> · 窗口 <span className="font-mono">{linkedRunId}</span></>}
            。下方「窗口状态」「构建时序链路」已自动载入。
          </span>
        </div>
      )}

      {/* 开启窗口 */}
      <section>
        <div className="mb-2 flex items-center gap-2 text-sm font-semibold">
          <Flag className="h-4 w-4 text-primary" />
          开启行为窗口
        </div>
        <div className="grid grid-cols-2 gap-2 md:grid-cols-3">
          <label className="flex flex-col gap-1 text-xs text-muted-foreground">
            功能名（必填）
            <input className={inputCls} value={featureName} onChange={(e) => setFeatureName(e.target.value)} placeholder="login" />
          </label>
          <label className="flex flex-col gap-1 text-xs text-muted-foreground">
            项目路径（必填）
            <input className={inputCls} value={projectPath} onChange={(e) => setProjectPath(e.target.value)} placeholder="/path/to/repo" />
          </label>
          <label className="flex flex-col gap-1 text-xs text-muted-foreground">
            插件名（可选）
            <input className={inputCls} value={pluginName} onChange={(e) => setPluginName(e.target.value)} placeholder="http" />
          </label>
          <label className="flex flex-col gap-1 text-xs text-muted-foreground">
            设备（可选）
            <input className={inputCls} value={device} onChange={(e) => setDevice(e.target.value)} />
          </label>
          <label className="flex flex-col gap-1 text-xs text-muted-foreground">
            过滤（可选）
            <input className={inputCls} value={filter} onChange={(e) => setFilter(e.target.value)} placeholder="tcp port 8080" />
          </label>
          <label className="flex flex-col gap-1 text-xs text-muted-foreground">
            端口（可选）
            <input className={inputCls} value={port} onChange={(e) => setPort(e.target.value)} inputMode="numeric" />
          </label>
        </div>
        <div className="mt-2">
          <Button size="sm" onClick={handleBegin} disabled={!featureName || !projectPath || begin.isPending}>
            {begin.isPending ? "开启中…" : "开启窗口"}
          </Button>
        </div>
        {beginResult && (
          <div className="mt-2 rounded-md border border-border p-2 text-xs">
            <div className="flex items-center gap-2">
              <span className="font-mono">{beginResult.run_id}</span>
              <span className="rounded bg-primary-muted px-1.5 py-0.5 text-[11px] text-accent-foreground">
                {beginResult.capture_status}
              </span>
              {beginResult.uncertainties && beginResult.uncertainties.length > 0 && (
                <span className="text-amber-600">⚠ {beginResult.uncertainties.length} 项不确定</span>
              )}
            </div>
            <p className="mt-1 text-muted-foreground">
              隔离模式：{beginResult.capture_isolation_mode} · 会话 {beginResult.session_id}
            </p>
          </div>
        )}
      </section>

      {/* 当前窗口状态 + 关闭 */}
      {runId && (
        <section>
          <div className="mb-2 flex items-center justify-between">
            <div className="flex items-center gap-2 text-sm font-semibold">
              <Activity className="h-4 w-4 text-primary" />
              窗口状态
            </div>
            <Button size="sm" variant="outline" onClick={handleEnd} disabled={end.isPending}>
              <Square className="h-3.5 w-3.5" />
              {end.isPending ? "关闭中…" : "关闭窗口"}
            </Button>
          </div>

          {status.isLoading ? (
            <Skeleton className="h-24 w-full rounded-lg" />
          ) : statusData ? (
            <div className="gta-card space-y-1 p-3">
              <div className="flex items-center justify-between">
                <span className="font-mono text-xs">{runId}</span>
                <span className={`rounded-full border px-2 py-0.5 text-[11px] ${statusTone(statusData.status)}`}>
                  {statusData.status}
                </span>
              </div>
              <StatRow label="流数" value={statusData.flow_count} />
              <StatRow label="客户端请求" value={statusData.client_request_count} />
              <StatRow label="服务端消息" value={statusData.server_message_count} />
              <StatRow label="解码错误" value={statusData.decode_error_count} />
              <p className="pt-1 text-[11px] text-muted-foreground">起始 {statusData.time_from}</p>
            </div>
          ) : null}
        </section>
      )}

      {/* 构建行为链 */}
      {runId && (
        <section>
          <div className="mb-2 flex items-center gap-2 text-sm font-semibold">
            <GitBranch className="h-4 w-4 text-primary" />
            构建时序链路
          </div>
          <div className="flex flex-wrap items-end gap-2">
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              flow_id
              <input className={inputCls} value={flowId} onChange={(e) => setFlowId(e.target.value)} placeholder="flow-xxxx" />
            </label>
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              功能名
              <input className={inputCls} value={traceFeature} onChange={(e) => setTraceFeature(e.target.value)} />
            </label>
            <Button size="sm" onClick={handleTrace} disabled={!flowId || !traceFeature || trace.isPending}>
              {trace.isPending ? "构建中…" : "构建行为链"}
            </Button>
          </div>

          {traceResult && (
            <div className="mt-3 space-y-2">
              <div className="flex items-center gap-2 text-xs text-muted-foreground">
                <span>时间窗 {traceResult.time_window.from} → {traceResult.time_window.to}</span>
                {traceResult.file_path && <span className="font-mono text-info">{traceResult.file_path}</span>}
              </div>
              {traceResult.steps.length === 0 ? (
                <p className="text-xs text-muted-foreground">该 flow 无可用步骤。</p>
              ) : (
                <ol className="space-y-2">
                  {traceResult.steps.map((s, i) => (
                    <li key={s.step_id} className="gta-card p-3">
                      <div className="flex items-center gap-2 text-xs">
                        <span className="rounded bg-muted px-1.5 py-0.5 font-mono">#{i + 1}</span>
                        <span className="font-medium">{s.request.name}</span>
                        <span className="text-muted-foreground">→ {s.request.direction}</span>
                        {s.response && (
                          <span className="rounded bg-success/10 px-1.5 py-0.5 text-[11px] text-success">
                            {s.response.name}
                          </span>
                        )}
                      </div>
                      {s.pushes && s.pushes.length > 0 && (
                        <div className="mt-1 text-[11px] text-muted-foreground">
                          推送：{s.pushes.map((p) => `${p.name}(${p.msg_id})`).join(", ")}
                        </div>
                      )}
                      {s.entity_diffs && s.entity_diffs.length > 0 && (
                        <div className="mt-1 text-[11px] text-muted-foreground">
                          实体变更：{s.entity_diffs.map((d) => `${d.uri}#${d.key}`).join(", ")}
                        </div>
                      )}
                      <p className="mt-1 text-[11px] text-info">关联：{s.why_related}</p>
                    </li>
                  ))}
                </ol>
              )}
              {traceResult.close_info && (
                <div className="rounded-md border border-border p-2 text-xs text-muted-foreground">
                  关闭：{traceResult.close_info.closer} · {traceResult.close_info.method}
                  {traceResult.close_info.note ? ` — ${traceResult.close_info.note}` : ""}
                </div>
              )}
              {traceResult.uncertainties && traceResult.uncertainties.length > 0 && (
                <div className="flex items-center gap-1.5 text-[11px] text-amber-600">
                  <Info className="h-3.5 w-3.5" />
                  {traceResult.uncertainties.length} 项不确定
                </div>
              )}
            </div>
          )}
        </section>
      )}
    </div>
  );
}
