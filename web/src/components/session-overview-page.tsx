// SessionOverviewPage — 会话工作区的「概览」入口（P0-5 Session 收口）。
//
// 把 Session 从「一次抓包记录」提升为「一次调试工作单元」的默认落地页：
// 回答用户点进一个会话时最关心的四件事——会话什么状态、抓了多久/多少、
// 最近产生了什么（连接 / 协议事件）、下一步去哪分析（Connections / Timeline / 数据）。
// 不做新的数据模型，仅聚合既有查询（get_session_status / list_all_sessions /
// list_connections / list_decoded_data）。
import { Cable, Timeline as TimelineIcon, Table2, Wifi, AlertTriangle, ArrowRight } from "lucide-react";
import { useSessionStatus, useSessions, useConnections, useDecodedData } from "@/hooks/use-mcp";
import { RAW_DEBUG_ENABLED } from "@/lib/env";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import { Badge } from "@/components/ui/badge";

/** 概览页可跳转的分析视图（与 App 的 ViewTab 对齐的子集）。 */
export type OverviewTargetTab = "connections" | "timeline" | "decoded" | "raw";

interface SessionOverviewPageProps {
  sessionId: string | null;
  onNavigate: (tab: OverviewTargetTab) => void;
}

function fmtTime(iso?: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  return `${d.getMonth() + 1}月${d.getDate()}日 ${hh}:${mm}`;
}

function fmtDuration(sec?: number): string {
  if (!sec || sec <= 0) return "—";
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = Math.floor(sec % 60);
  if (h > 0) return `${h}时${m}分`;
  if (m > 0) return `${m}分${s}秒`;
  return `${s}秒`;
}

function fmtNum(n?: number): string {
  return (n ?? 0).toLocaleString();
}

/** 单个统计卡片。 */
function StatCard({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div className="rounded-xl border border-border bg-card/60 px-3 py-2.5">
      <p className="text-[11px] text-muted-foreground">{label}</p>
      <p className="mt-0.5 truncate font-mono text-sm font-medium text-foreground" title={hint ?? value}>
        {value}
      </p>
    </div>
  );
}

export function SessionOverviewPage({ sessionId, onNavigate }: SessionOverviewPageProps) {
  const { data: sessionsData, isLoading: sessionsLoading } = useSessions();
  const meta = sessionsData?.sessions?.find((s) => s.session_id === sessionId);
  const { data: status } = useSessionStatus(sessionId);
  const { data: connectionsData } = useConnections(sessionId, { limit: 5 });
  const { data: eventsData } = useDecodedData(sessionId ?? null, { limit: 5 });

  if (!sessionId) {
    return (
      <div className="flex h-full items-center justify-center p-6">
        <EmptyState
          icon={<Cable className="h-5 w-5" />}
          title="选择一个会话"
          hint="从左侧会话列表选择要分析的抓包会话，这里会展示它的概览与最近数据。"
        />
      </div>
    );
  }

  if (sessionsLoading && !meta) {
    return (
      <div className="mx-auto max-w-4xl space-y-4 p-6">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }

  // —— 派生状态（gRPC 实时态优先，降级用会话元数据）——
  const isAgentSource = meta?.source === "agent";
  const running = (status?.state ?? meta?.status) === "running";
  const packetsIn = (status?.packets_in ?? 0) + (status?.raw_count ?? 0);
  const rawCount = packetsIn > 0 ? packetsIn : (status?.raw_packets ?? meta?.raw_packets ?? 0);
  const eventCount = status?.event_count ?? status?.events ?? meta?.events ?? 0;
  const decodeErrors = status?.decode_errors ?? meta?.decode_errors ?? 0;
  // Agent 源会话：运行中但还没有任何数据流入 → 等待 Agent 接入。
  const waitingAgent = isAgentSource && running && rawCount === 0;
  const decodeTotal = eventCount + decodeErrors;
  const decodeRate =
    decodeTotal > 0 ? `${Math.round((eventCount / decodeTotal) * 100)}%` : "—";
  // 运行中的会话实时计算持续时间（每次轮询重渲染会刷新）。
  const liveDuration =
    running && meta?.started_at
      ? Math.max(
          0,
          Math.floor((Date.now() - new Date(meta.started_at).getTime()) / 1000),
        )
      : (status?.duration_sec ?? meta?.duration_sec ?? 0);
  const connectionCount = connectionsData?.count ?? 0;
  const sourceLabel = isAgentSource
    ? "远程 Agent"
    : meta?.source === "proxy"
      ? "移动代理"
      : "服务器网卡";

  const statusBadge = waitingAgent ? (
    <Badge variant="outline" className="gap-1 border-amber-500/50 text-amber-600 dark:text-amber-300">
      <Wifi className="h-3 w-3 animate-pulse" />
      等待 Agent 接入
    </Badge>
  ) : running ? (
    <Badge variant="secondary" className="gap-1">
      <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-emerald-500" />
      抓包中
    </Badge>
  ) : (
    <Badge variant="outline">已停止</Badge>
  );

  const recentConnections = connectionsData?.connections ?? [];
  const recentEvents = eventsData?.events ?? [];

  return (
    <div className="h-full overflow-auto gta-scroll">
      <div className="mx-auto max-w-4xl space-y-5 p-6">
        {/* 头部：会话身份 + 状态 */}
        <header className="flex flex-wrap items-center justify-between gap-3">
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2">
              <h1 className="font-mono text-sm font-semibold text-foreground">
                {sessionId}
              </h1>
              {statusBadge}
              {meta?.plugin && <Badge variant="outline">{meta.plugin}</Badge>}
            </div>
            <p className="mt-1 text-xs text-muted-foreground">
              {sourceLabel}
              {meta?.port ? ` · 端口 ${meta.port}` : ""}
              {meta?.owner ? ` · ${meta.owner}` : ""}
              {meta?.project_id ? ` · 项目 ${meta.project_id}` : ""}
            </p>
          </div>
          <div className="flex flex-wrap gap-1.5">
            <Button variant="outline" size="sm" className="h-8" onClick={() => onNavigate("connections")}>
              <Cable className="h-3.5 w-3.5" />
              查看连接
            </Button>
            <Button variant="outline" size="sm" className="h-8" onClick={() => onNavigate("timeline")}>
              <TimelineIcon className="h-3.5 w-3.5" />
              时间线
            </Button>
            <Button variant="outline" size="sm" className="h-8" onClick={() => onNavigate("decoded")}>
              <Table2 className="h-3.5 w-3.5" />
              协议数据
            </Button>
            {RAW_DEBUG_ENABLED && (
              <Button variant="outline" size="sm" className="h-8" onClick={() => onNavigate("raw")}>
                原始包
              </Button>
            )}
          </div>
        </header>

        {waitingAgent && (
          <div className="flex items-center gap-2 rounded-xl border border-amber-500/40 bg-amber-500/10 px-3 py-2.5 text-sm text-amber-700 dark:text-amber-300">
            <Wifi className="h-4 w-4 shrink-0 animate-pulse" />
            会话已创建，等待成员机 Agent 连接：Agent 上线推流后此处会自动变为「抓包中」。
          </div>
        )}

        {decodeErrors > 0 && (
          <div className="flex items-center gap-2 rounded-xl border border-border bg-muted/50 px-3 py-2.5 text-sm text-muted-foreground">
            <AlertTriangle className="h-4 w-4 shrink-0" />
            本次会话有 {fmtNum(decodeErrors)} 条解码失败（可能是非目标协议流量或插件不匹配）。
          </div>
        )}

        {/* 统计卡片 */}
        <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
          <StatCard label="开始时间" value={fmtTime(meta?.started_at)} />
          <StatCard label="持续时间" value={fmtDuration(liveDuration)} />
          <StatCard label="原始包" value={fmtNum(rawCount)} />
          <StatCard label="解码事件" value={fmtNum(eventCount)} />
          <StatCard label="连接数" value={fmtNum(connectionCount)} />
          <StatCard label="解析成功率" value={decodeRate} hint="解码事件 / (解码事件 + 解码错误)" />
          <StatCard label="解码错误" value={fmtNum(decodeErrors)} />
          <StatCard label="抓包源" value={sourceLabel} />
        </div>

        {/* 最近连接 */}
        <section>
          <div className="flex items-center justify-between">
            <h2 className="text-sm font-medium text-foreground">最近连接</h2>
            {recentConnections.length > 0 && (
              <button
                type="button"
                onClick={() => onNavigate("connections")}
                className="inline-flex items-center gap-0.5 text-xs text-muted-foreground hover:text-foreground"
              >
                全部 {fmtNum(connectionCount)} 条
                <ArrowRight className="h-3 w-3" />
              </button>
            )}
          </div>
          <div className="mt-2 rounded-2xl border border-border bg-card/60 p-3">
            {recentConnections.length === 0 ? (
              <p className="px-1 py-2 text-sm text-muted-foreground">暂无连接数据。</p>
            ) : (
              <ul className="space-y-1">
                {recentConnections.map((c) => (
                  <li key={c.conn_id}>
                    <button
                      type="button"
                      onClick={() => onNavigate("connections")}
                      className="flex w-full items-center gap-3 rounded-lg px-2 py-1.5 text-left hover:bg-muted/40"
                    >
                      <div className="min-w-0 flex-1">
                        <p className="truncate font-mono text-xs text-foreground">
                          {c.client} → {c.server}
                        </p>
                        <p className="truncate text-[11px] text-muted-foreground">
                          {c.protocol} · {fmtTime(c.start_time)} · {fmtDuration(c.duration_sec)}
                        </p>
                      </div>
                      <span className="shrink-0 font-mono text-xs text-muted-foreground">
                        {fmtNum(c.event_count)} events
                      </span>
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </section>

        {/* 最近协议事件 */}
        <section>
          <div className="flex items-center justify-between">
            <h2 className="text-sm font-medium text-foreground">最近协议事件</h2>
            {recentEvents.length > 0 && (
              <button
                type="button"
                onClick={() => onNavigate("decoded")}
                className="inline-flex items-center gap-0.5 text-xs text-muted-foreground hover:text-foreground"
              >
                查看全部
                <ArrowRight className="h-3 w-3" />
              </button>
            )}
          </div>
          <div className="mt-2 rounded-2xl border border-border bg-card/60 p-3">
            {recentEvents.length === 0 ? (
              <p className="px-1 py-2 text-sm text-muted-foreground">
                暂无解码事件{meta?.plugin ? "" : "（未指定解析插件，仅抓包不解码）"}。
              </p>
            ) : (
              <ul className="space-y-1">
                {recentEvents.map((ev) => (
                  <li key={ev.id}>
                    <button
                      type="button"
                      onClick={() => onNavigate("decoded")}
                      className="flex w-full items-center gap-3 rounded-lg px-2 py-1.5 text-left hover:bg-muted/40"
                    >
                      <Badge variant="outline" className="shrink-0 font-mono text-[10px]">
                        {ev.protocol}
                      </Badge>
                      <span className="min-w-0 flex-1 truncate font-mono text-xs text-foreground">
                        {ev.id}
                      </span>
                      <span className="shrink-0 text-[11px] text-muted-foreground">
                        {fmtTime(ev.timestamp)}
                      </span>
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </section>
      </div>
    </div>
  );
}
