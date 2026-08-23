import { useState } from "react";
import { cn } from "@/lib/utils";
import {
  useSessions,
  useSetSessionPlugin,
  useListPlugins,
  useSessionStatus,
  useDeleteSession,
} from "@/hooks/use-mcp";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import { Dialog } from "@/components/ui/dialog";
import { toast } from "@/components/ui/toast";
import {
  Activity,
  FileText,
  Wifi,
  Cable,
  RefreshCw,
  X,
  Inbox,
  AlertTriangle,
  RotateCw,
  Trash2,
} from "lucide-react";
import type { SessionInfo } from "@/types/session";
import type { SessionStatusResult } from "@/types/session-extra";

interface SessionSidebarProps {
  selectedSessionId: string | null;
  onSelectSession: (sessionId: string) => void;
  /** 会话被删除后回调（用于清空选中态，避免后续查询打到已删除的会话） */
  onDeleted?: (sessionId: string) => void;
}

/** 格式化数字，加千分位 */
function formatNumber(n: number): string {
  if (n === 0) return "0";
  return n.toLocaleString();
}

/** 将 RFC3339 时间格式化为 "MM/DD HH:mm:ss" */
function formatTime(isoStr: string): string {
  if (!isoStr) return "";
  const d = new Date(isoStr);
  if (Number.isNaN(d.getTime())) return isoStr;
  const mm = String(d.getMonth() + 1).padStart(2, "0");
  const dd = String(d.getDate()).padStart(2, "0");
  const hh = String(d.getHours()).padStart(2, "0");
  const mi = String(d.getMinutes()).padStart(2, "0");
  const ss = String(d.getSeconds()).padStart(2, "0");
  return `${mm}/${dd} ${hh}:${mi}:${ss}`;
}

/** 从 RFC3339 提取 "HH:mm:ss"（同日省略日期） */
function formatTimeOnly(isoStr: string): string {
  if (!isoStr) return "";
  const d = new Date(isoStr);
  if (Number.isNaN(d.getTime())) return isoStr;
  const hh = String(d.getHours()).padStart(2, "0");
  const mi = String(d.getMinutes()).padStart(2, "0");
  const ss = String(d.getSeconds()).padStart(2, "0");
  return `${hh}:${mi}:${ss}`;
}

/** 判断两个 ISO 时间是否同一天 */
function isSameDay(a: string, b: string): boolean {
  if (!a || !b) return false;
  const da = new Date(a);
  const db = new Date(b);
  if (Number.isNaN(da.getTime()) || Number.isNaN(db.getTime())) return false;
  return (
    da.getFullYear() === db.getFullYear() &&
    da.getMonth() === db.getMonth() &&
    da.getDate() === db.getDate()
  );
}

/** 秒数格式化为 "1m 23s" / "1h 5m" / "125s" */
function formatDuration(sec: number): string {
  if (sec <= 0) return "";
  if (sec < 60) return `${sec.toFixed(0)}s`;
  const m = Math.floor(sec / 60);
  const s = Math.floor(sec % 60);
  if (m < 60) return s > 0 ? `${m}m ${s}s` : `${m}m`;
  const h = Math.floor(m / 60);
  const rm = m % 60;
  return rm > 0 ? `${h}h ${rm}m` : `${h}h`;
}

/** 从 pcap_file 路径提取文件名 */
function basename(p: string): string {
  if (!p) return "";
  const parts = p.replace(/\\/g, "/").split("/");
  return parts[parts.length - 1] ?? p;
}

function SessionItem({
  session,
  isSelected,
  onClick,
  onSwitch,
  onDeleted,
  liveStatus,
}: {
  session: SessionInfo;
  isSelected: boolean;
  onClick: () => void;
  onSwitch?: (session: SessionInfo) => void;
  onDeleted?: (sessionId: string) => void;
  /** 仅对“当前选中会话”注入 get_session_status 实时态；其余为 undefined */
  liveStatus?: SessionStatusResult | null;
}) {
  const isRunning = session.status === "running";
  const hasStopped = !isRunning && !!session.stopped_at;

  // 删除会话（破坏性）：二次确认后才真正调用。
  const delSession = useDeleteSession();
  const [confirmOpen, setConfirmOpen] = useState(false);

  // 选中会话优先展示实时态计数（packets_in / event_count / decode_errors）。
  const liveEvents = liveStatus?.event_count ?? session.events;
  const liveRaw = liveStatus?.raw_count ?? liveStatus?.packets_in ?? session.raw_packets;
  const liveErrors = liveStatus?.decode_errors ?? session.decode_errors;

  // 来源：代理抓包用 "Mobile Proxy + 监听地址"，live 抓包用网卡名，文件回放用文件名
  const isFileReplay = !!session.pcap_file;
  const isProxy = session.source === "proxy";
  const sourceLabel = isProxy
    ? `Mobile Proxy${session.listen_addr ? ` · ${session.listen_addr}` : ""}`
    : isFileReplay
      ? basename(session.pcap_file)
      : session.interface || "(auto)";

  // 结束时间：同日省略日期
  const stoppedDisplay = hasStopped
    ? isSameDay(session.started_at, session.stopped_at)
      ? formatTimeOnly(session.stopped_at)
      : formatTime(session.stopped_at)
    : "";

  return (
    <div
      role="button"
      tabIndex={0}
      onClick={onClick}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onClick();
        }
      }}
      className={cn(
        "w-full text-left rounded-lg border p-3 transition-[border-color,box-shadow,background-color] hover:shadow-sm cursor-pointer outline-none focus-visible:ring-2 focus-visible:ring-ring/40",
        isSelected
          ? "border-primary bg-primary-muted shadow-sm"
          : "border-border bg-card hover:border-primary/40",
      )}
    >
      {/* 头部：状态点 + 开始时间 + Badge */}
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2 min-w-0">
          <span
            className={cn(
              "inline-block h-2 w-2 rounded-full shrink-0",
              isRunning ? "gta-live-dot" : "bg-muted-foreground/50",
            )}
          />
          <span className="text-sm font-medium truncate font-mono">
            {formatTime(session.started_at)}
          </span>
        </div>
        <Badge variant={isRunning ? "default" : "secondary"} className="shrink-0">
          {isRunning ? "运行中" : "已停止"}
        </Badge>
      </div>

      {/* 来源 + 端口 + 插件 */}
      <div className="mt-2 flex items-center gap-2 text-xs text-muted-foreground flex-wrap">
        {isFileReplay ? (
          <FileText className="h-3 w-3 shrink-0" />
        ) : isProxy ? (
          <Cable className="h-3 w-3 shrink-0" />
        ) : (
          <Wifi className="h-3 w-3 shrink-0" />
        )}
        <span className="truncate max-w-[120px]" title={sourceLabel}>
          {sourceLabel}
        </span>
        {session.port > 0 && <span className="font-mono">:{session.port}</span>}
        {session.plugin && (
          <>
            <span className="text-muted-foreground/50">·</span>
            <Activity className="h-3 w-3 shrink-0" />
            <span className="truncate">{session.plugin}</span>
          </>
        )}
      </div>

      {/* 已停止：结束时间 + 时长 */}
      {hasStopped && (
        <div className="mt-1.5 flex items-center gap-2 text-xs text-muted-foreground">
          <span className="font-mono">→ {stoppedDisplay}</span>
          {session.duration_sec > 0 && (
            <span className="text-muted-foreground/80">
              ({formatDuration(session.duration_sec)})
            </span>
          )}
        </div>
      )}

      {/* 底部统计（选中会话优先用实时态） */}
      <div className="mt-2 flex items-center gap-3 text-xs text-muted-foreground tabular-nums">
        <span>{formatNumber(liveEvents)} events</span>
        <span>{formatNumber(liveRaw)} packets</span>
        {liveErrors > 0 && (
          <span className="text-destructive">
            {formatNumber(liveErrors)} errors
          </span>
        )}
        {liveStatus && <span className="text-[11px] text-info">· 实时</span>}
      </div>

      {/* 操作行：运行中可切换插件；任何会话可删除（需二次确认） */}
      <div className="mt-2 flex items-center justify-end gap-2">
        {isRunning && onSwitch && (
          <Button
            size="sm"
            variant="outline"
            className="h-6 text-xs"
            onClick={(e) => {
              e.stopPropagation();
              onSwitch(session);
            }}
            onKeyDown={(e) => e.stopPropagation()}
          >
            <RefreshCw className="h-3 w-3 mr-1" />
            切换插件
          </Button>
        )}
        {onDeleted && (
          <Button
            size="sm"
            variant="ghost"
            className="h-6 text-xs text-destructive hover:text-destructive"
            title="删除会话"
            aria-label="删除会话"
            onClick={(e) => {
              e.stopPropagation();
              setConfirmOpen(true);
            }}
            onKeyDown={(e) => e.stopPropagation()}
          >
            <Trash2 className="h-3 w-3" />
          </Button>
        )}
      </div>

      {/* 删除确认对话框 */}
      {confirmOpen && (
        <Dialog
          open
          onClose={() => setConfirmOpen(false)}
          icon={<Trash2 className="h-5 w-5" />}
          title="删除会话"
          description="该操作会删除会话及其全部数据（原始包、事件、状态变更与投影），且不可恢复。"
          footer={
            <>
              <Button variant="outline" onClick={() => setConfirmOpen(false)}>
                <X className="h-4 w-4" />
                取消
              </Button>
              <Button
                variant="destructive"
                disabled={delSession.isPending}
                onClick={() =>
                  delSession.mutate(
                    { sessionId: session.session_id },
                    {
                      onSuccess: () => {
                        toast.success("会话已删除", session.session_id);
                        setConfirmOpen(false);
                        onDeleted?.(session.session_id);
                      },
                      onError: (err) => toast.error("删除失败", err.message),
                    },
                  )
                }
              >
                {delSession.isPending ? "删除中…" : "确认删除"}
              </Button>
            </>
          }
        >
          <div className="rounded-md bg-muted px-2 py-1.5 font-mono text-xs text-muted-foreground break-all">
            {session.session_id}
          </div>
        </Dialog>
      )}
    </div>
  );
}

export function SessionSidebar({
  selectedSessionId,
  onSelectSession,
  onDeleted,
}: SessionSidebarProps) {
  const { data, isLoading, isError, error, refetch } = useSessions();
  // 仅对当前选中会话拉取 get_session_status（5s 轮询），使其统计与状态点保持“实时”。
  const liveStatus = useSessionStatus(selectedSessionId);
  const [switchTarget, setSwitchTarget] = useState<SessionInfo | null>(null);

  const sessions = data?.sessions ?? [];

  // 排序：运行中优先，然后按 started_at 倒序（最新的在上）
  const sortedSessions = [...sessions].sort((a, b) => {
    const aRunning = a.status === "running" ? 1 : 0;
    const bRunning = b.status === "running" ? 1 : 0;
    if (aRunning !== bRunning) return bRunning - aRunning;
    return b.started_at.localeCompare(a.started_at);
  });

  const runningCount = sessions.filter((s) => s.status === "running").length;

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      {/* 标题 */}
      <div className="flex items-center justify-between gap-2 px-4 py-3 border-b">
        <h2 className="text-sm font-semibold">会话列表</h2>
        {data && (
          <span className="inline-flex items-center gap-1.5 rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground">
            {runningCount > 0 && <span className="gta-live-dot" />}
            {data.count} 个会话{runningCount > 0 && ` · ${runningCount} 运行`}
          </span>
        )}
      </div>

      {/* 会话列表 */}
      <ScrollArea className="flex-1 p-3">
        {isLoading && (
          <div className="space-y-3">
            {Array.from({ length: 3 }).map((_, i) => (
              <Skeleton key={i} className="h-24 w-full rounded-lg" />
            ))}
          </div>
        )}

        {isError && (
          <div
            role="alert"
            className="flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive"
          >
            <AlertTriangle className="h-4 w-4 shrink-0" />
            <span className="flex-1">加载失败：{error?.message ?? "未知错误"}</span>
            <Button variant="outline" size="sm" onClick={() => refetch()} className="h-7">
              <RotateCw className="h-3.5 w-3.5" />
              重试
            </Button>
          </div>
        )}

        {!isLoading && !isError && sessions.length === 0 && (
          <EmptyState
            icon={<Inbox className="h-5 w-5" />}
            title="暂无会话"
            hint="点击右上角「开始抓包」启动一次抓包会话，或启动插件进行离线解码。"
          />
        )}

        <div className="space-y-2">
          {sortedSessions.map((session) => (
            <SessionItem
              key={session.session_id}
              session={session}
              isSelected={session.session_id === selectedSessionId}
              onClick={() => onSelectSession(session.session_id)}
              onSwitch={setSwitchTarget}
              onDeleted={onDeleted}
              liveStatus={
                session.session_id === selectedSessionId ? (liveStatus.data ?? null) : null
              }
            />
          ))}
        </div>
      </ScrollArea>

      {switchTarget && (
        <SwitchPluginDialog session={switchTarget} onClose={() => setSwitchTarget(null)} />
      )}
    </div>
  );
}

/** 切换运行中会话的解码插件绑定对话框。 */
function SwitchPluginDialog({
  session,
  onClose,
}: {
  session: SessionInfo;
  onClose: () => void;
}) {
  const setPlugin = useSetSessionPlugin();
  const { data: pluginsData } = useListPlugins();
  const plugins = pluginsData?.plugins ?? [];
  const [plugin, setPluginName] = useState(session.plugin ?? "");

  function handleSwitch() {
    if (!plugin) return;
    setPlugin.mutate(
      { sessionId: session.session_id, plugin },
      {
        onSuccess: () => {
          toast.success("已切换解码插件", `会话 ${session.session_id} → ${plugin}`);
          onClose();
        },
        onError: (err) => {
          toast.error("切换失败", err.message);
        },
      },
    );
  }

  return (
    <Dialog
      open
      onClose={onClose}
      icon={<RefreshCw className="h-5 w-5" />}
      title="切换解码插件"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            <X className="h-4 w-4" />
            取消
          </Button>
          <Button onClick={handleSwitch} disabled={setPlugin.isPending || !plugin}>
            {setPlugin.isPending ? "切换中…" : "切换"}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <div className="rounded-md bg-muted px-2 py-1.5 font-mono text-xs text-muted-foreground break-all">
          {session.session_id}
        </div>
        <div>
          <label className="text-sm font-medium">解码插件</label>
          <select
            className="mt-1.5 h-8 w-full rounded-md border border-input bg-background px-2 text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
            value={plugin}
            onChange={(e) => setPluginName(e.target.value)}
          >
            {plugins.length === 0 ? (
              <option value="">无可用插件</option>
            ) : (
              plugins.map((pl) => (
                <option key={pl} value={pl}>
                  {pl}
                </option>
              ))
            )}
          </select>
          <p className="mt-1 text-xs text-muted-foreground">
            切换立即生效，下一个包即由新插件解码，无需停止抓包。
          </p>
        </div>
        {setPlugin.isError && (
          <p className="text-xs text-destructive">
            切换失败：{setPlugin.error?.message}
          </p>
        )}
      </div>
    </Dialog>
  );
}
