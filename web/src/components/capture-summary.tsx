import { useSessions } from "@/hooks/use-mcp";
import { CheckCircle2, AlertTriangle } from "lucide-react";

function sourceName(source: string): string {
  if (source === "agent") return "远程 Agent";
  if (source === "proxy") return "手机代理";
  return "服务器抓包";
}

/**
 * 「本次抓包」结果摘要：回答用户最关心的问题——这次抓包到底成功没有。
 * 只消费已加载的 session 元数据 + 连接数，不额外拉时间线，保持轻量。
 */
export function CaptureSummary({
  sessionId,
  connectionCount,
}: {
  sessionId: string;
  connectionCount: number;
}) {
  const { data } = useSessions();
  const session = data?.sessions?.find((s) => s.session_id === sessionId);
  if (!session) return null;

  const packets = session.raw_packets ?? 0;
  const events = session.events ?? 0;
  const decodeErrors = session.decode_errors ?? 0;
  const hasData = packets > 0 || events > 0 || connectionCount > 0;
  const running = session.status === "running";

  if (!hasData) {
    return (
      <div className="rounded-xl border border-amber-300/50 bg-amber-50 px-4 py-3 dark:border-amber-500/40 dark:bg-amber-500/10">
        <div className="flex items-center gap-2 text-sm font-medium text-amber-800 dark:text-amber-200">
          <AlertTriangle className="h-4 w-4 shrink-0" />
          抓包没有产生有效数据
        </div>
        <p className="mt-1 text-xs leading-relaxed text-amber-700 dark:text-amber-300">
          {running
            ? "抓包仍在进行，但尚未收到任何流量。"
            : "本次抓包未捕获到任何流量。"}
          原因：未检测到端口 {session.port || "未知"} 的流量。
          建议：确认游戏实际使用的端口与抓包端口一致，再重新抓包。
        </p>
      </div>
    );
  }

  const items: { label: string; value: string; warn?: boolean }[] = [
    { label: "来源", value: sourceName(session.source) },
    { label: "Packets", value: packets.toLocaleString() },
    { label: "Events", value: events.toLocaleString() },
    { label: "连接", value: connectionCount.toLocaleString() },
    { label: "解码错误", value: decodeErrors.toLocaleString(), warn: decodeErrors > 0 },
  ];

  return (
    <div className="rounded-xl border border-border bg-card/60 px-4 py-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="flex items-center gap-1.5 text-sm font-medium text-foreground">
          <CheckCircle2 className="h-4 w-4 text-emerald-500" />
          本次抓包
        </span>
        {session.port ? (
          <span className="text-xs text-muted-foreground">端口 {session.port}</span>
        ) : null}
        {session.plugin ? (
          <span className="rounded bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
            {session.plugin}
          </span>
        ) : null}
        {running ? (
          <span className="rounded bg-emerald-500/10 px-1.5 py-0.5 text-[10px] font-medium text-emerald-600 dark:text-emerald-400">
            抓包中
          </span>
        ) : null}
      </div>
      <div className="mt-2 flex flex-wrap gap-x-5 gap-y-1.5">
        {items.map((it) => (
          <div key={it.label} className="flex items-baseline gap-1.5">
            <span className="text-xs text-muted-foreground">{it.label}</span>
            <span
              className={`font-mono text-sm tabular-nums ${
                it.warn ? "text-red-600 dark:text-red-400" : "text-foreground"
              }`}
            >
              {it.value}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}
