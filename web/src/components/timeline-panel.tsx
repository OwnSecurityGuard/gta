import { useMemo, useState } from "react";
import { useSessionTimeline } from "@/hooks/use-mcp";
import type { TimelineNode } from "@/types/timeline";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import {
  GitBranch,
  Network,
  AlertTriangle,
  ChevronRight,
  ChevronDown,
  ArrowRight,
  ArrowLeft,
  MessagesSquare,
  X,
} from "lucide-react";

// ─── 方向徽标 ────────────────────────────────────────────────

function DirectionBadge({ direction }: { direction: string }) {
  switch (direction) {
    case "client_to_server":
      return (
        <span className="inline-flex shrink-0 items-center gap-0.5 rounded-full bg-blue-50 px-2 py-0.5 text-xs font-medium text-blue-700 dark:bg-blue-950 dark:text-blue-300">
          <ArrowRight className="h-3 w-3" />
          C→S
        </span>
      );
    case "server_to_client":
      return (
        <span className="inline-flex shrink-0 items-center gap-0.5 rounded-full bg-emerald-50 px-2 py-0.5 text-xs font-medium text-emerald-700 dark:bg-emerald-950 dark:text-emerald-300">
          <ArrowLeft className="h-3 w-3" />
          S→C
        </span>
      );
    default:
      return (
        <span className="inline-flex shrink-0 items-center rounded-full bg-muted px-2 py-0.5 text-xs text-muted-foreground">
          ?
        </span>
      );
  }
}

function formatTimestamp(isoStr: string): string {
  try {
    return new Date(isoStr).toLocaleString("zh-CN", {
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    });
  } catch {
    return isoStr;
  }
}

function nodeLabel(node: TimelineNode): string {
  return node.msg_name || node.type || "(unknown)";
}

// ─── 时间线树节点（递归） ────────────────────────────────────

function TimelineNodeView({
  node,
  depth,
  expandedIds,
  onToggle,
}: {
  node: TimelineNode;
  depth: number;
  expandedIds: Set<string>;
  onToggle: (id: string) => void;
}) {
  const children = node.children ?? [];
  const hasChildren = children.length > 0;
  const isExpanded = expandedIds.has(node.id);
  const summary = node.summary ?? "";

  return (
    <div>
      <div
        role="button"
        tabIndex={0}
        aria-expanded={hasChildren ? isExpanded : undefined}
        onClick={() => onToggle(node.id)}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            onToggle(node.id);
          }
        }}
        className="group flex cursor-pointer items-center gap-1.5 rounded px-1 py-1 hover:bg-muted/40"
        style={{ paddingLeft: `${depth * 22 + 6}px` }}
      >
        {hasChildren ? (
          isExpanded ? (
            <ChevronDown className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
          ) : (
            <ChevronRight className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
          )
        ) : (
          <span className="h-3.5 w-3.5 shrink-0" />
        )}
        <span className="w-16 shrink-0 font-mono text-[11px] tabular-nums text-muted-foreground">
          {formatTimestamp(node.timestamp)}
        </span>
        <DirectionBadge direction={node.direction ?? ""} />
        <span
          className="min-w-0 flex-1 truncate font-mono text-xs font-medium"
          title={summary || node.id}
        >
          {nodeLabel(node)}
        </span>
        {node.is_push && (
          <span className="shrink-0 rounded bg-primary/10 px-1 py-px text-[10px] font-medium uppercase text-primary">
            push
          </span>
        )}
        {node.correlation_id && (
          <span
            className="shrink-0 rounded bg-muted px-1.5 py-px font-mono text-[10px] text-muted-foreground"
            title={`correlation_id: ${node.correlation_id}`}
          >
            #{node.correlation_id.slice(0, 8)}
          </span>
        )}
        <span className="shrink-0 font-mono text-[10px] text-muted-foreground/60" title={node.id}>
          {node.id.slice(0, 8)}
        </span>
      </div>
      {hasChildren && isExpanded && (
        <div>
          {children.map((c) => (
            <TimelineNodeView
              key={c.id}
              node={c}
              depth={depth + 1}
              expandedIds={expandedIds}
              onToggle={onToggle}
            />
          ))}
        </div>
      )}
    </div>
  );
}

// 在根树中收集某 correlation_id 下的事件（按时间戳升序），用于"对话"视图。
function collectByCorrelation(roots: TimelineNode[], corr: string): TimelineNode[] {
  const out: TimelineNode[] = [];
  const walk = (nodes: TimelineNode[] | undefined) => {
    for (const n of nodes ?? []) {
      if (n.correlation_id === corr) out.push(n);
      walk(n.children);
    }
  };
  walk(roots);
  return out.sort((a, b) => a.timestamp.localeCompare(b.timestamp));
}

// ─── 主面板 ──────────────────────────────────────────────────

export function TimelinePanel({ sessionId }: { sessionId: string | null }) {
  const [limit, setLimit] = useState(500);
  const [expandedIds, setExpandedIds] = useState<Set<string>>(new Set());
  const [activeCorrelation, setActiveCorrelation] = useState<string | null>(null);

  const { data, isLoading, isError, error } = useSessionTimeline(sessionId, { limit });

  const roots = data?.roots ?? [];
  const conversations = useMemo(() => {
    const list = data?.conversations ?? [];
    return [...list].sort((a, b) => b.event_count - a.event_count);
  }, [data]);

  const conversationEvents = useMemo(() => {
    if (!activeCorrelation) return [];
    return collectByCorrelation(roots, activeCorrelation);
  }, [roots, activeCorrelation]);

  function toggleNode(id: string) {
    setExpandedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  function expandAll() {
    const ids = new Set<string>();
    const walk = (nodes: TimelineNode[] | undefined) => {
      for (const n of nodes ?? []) {
        if ((n.children?.length ?? 0) > 0) ids.add(n.id);
        walk(n.children);
      }
    };
    walk(roots);
    setExpandedIds(ids);
  }

  if (!sessionId) {
    return (
      <EmptyState
        icon={<Network className="h-5 w-5" />}
        title="未选择会话"
        hint="在左侧选择一个会话后，这里会展示整次抓包的请求/响应因果树与对话分组（get_session_timeline）。"
      />
    );
  }

  return (
    <div className="h-full overflow-auto p-4 gta-scroll space-y-5">
      {/* 会话上下文 */}
      <section>
        <div className="mb-2 flex items-center gap-2 text-sm font-semibold">
          <GitBranch className="h-4 w-4 text-primary" />
          会话时间线
        </div>
        {data && (
          <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
            <span>
              事件 <span className="font-mono tabular-nums">{data.event_count}</span>
            </span>
            <span>
              根节点 <span className="font-mono tabular-nums">{data.root_count}</span>
            </span>
            {data.plugin && (
              <span className="rounded bg-muted px-1.5 py-0.5 font-mono">{data.plugin}</span>
            )}
            {data.status && (
              <span className="rounded bg-muted px-1.5 py-0.5">{data.status}</span>
            )}
            <label className="ml-auto flex items-center gap-1">
              窗口
              <select
                className="h-7 rounded-md border border-input bg-background px-1.5 text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
                value={limit}
                onChange={(e) => setLimit(Number(e.target.value))}
              >
                <option value={200}>200</option>
                <option value={500}>500</option>
                <option value={1000}>1000</option>
                <option value={5000}>5000</option>
              </select>
            </label>
          </div>
        )}
        {(data?.uncertainties ?? []).length > 0 && (
          <div className="mt-2 space-y-1">
            {(data?.uncertainties ?? []).map((u, i) => (
              <div
                key={i}
                className="flex items-center gap-1.5 rounded-md border border-amber-300/40 bg-amber-50 px-2 py-1 text-[11px] text-amber-700 dark:bg-amber-950/40 dark:text-amber-300"
              >
                <AlertTriangle className="h-3.5 w-3.5 shrink-0" />
                <span>{u}</span>
              </div>
            ))}
          </div>
        )}
      </section>

      {/* 对话分组 */}
      <section>
        <div className="mb-2 flex items-center gap-2 text-sm font-semibold">
          <MessagesSquare className="h-4 w-4 text-primary" />
          对话分组
          <span className="text-xs font-normal text-muted-foreground">
            同一 correlation_id 的请求/响应聚合
          </span>
        </div>
        {conversations.length === 0 ? (
          <p className="text-xs text-muted-foreground">无对话分组（事件未携带 correlation_id）。</p>
        ) : (
          <div className="flex flex-wrap gap-1.5">
            {conversations.map((c) => {
              const active = activeCorrelation === c.correlation_id;
              return (
                <button
                  key={c.correlation_id}
                  type="button"
                  onClick={() =>
                    setActiveCorrelation(active ? null : c.correlation_id)
                  }
                  className={
                    "rounded-full border px-2.5 py-1 font-mono text-xs transition-colors " +
                    (active
                      ? "border-primary bg-primary-muted text-accent-foreground"
                      : "border-border bg-background text-muted-foreground hover:border-primary/40 hover:text-foreground")
                  }
                  title={c.correlation_id}
                >
                  #{c.correlation_id.slice(0, 10)} · {c.event_count}
                </button>
              );
            })}
          </div>
        )}
      </section>

      {/* 时间线 / 对话详情 */}
      <section>
        {activeCorrelation ? (
          <div>
            <div className="mb-2 flex items-center justify-between">
              <div className="flex items-center gap-2 text-sm font-semibold">
                <MessagesSquare className="h-4 w-4 text-primary" />
                对话
                <span className="font-mono text-xs text-muted-foreground">
                  #{activeCorrelation.slice(0, 12)}
                </span>
                <span className="text-xs font-normal text-muted-foreground">
                  {conversationEvents.length} 条事件
                </span>
              </div>
              <button
                type="button"
                onClick={() => setActiveCorrelation(null)}
                className="inline-flex items-center gap-1 rounded-md border border-input px-2 py-1 text-xs hover:bg-muted/40"
              >
                <X className="h-3 w-3" />
                返回完整时间线
              </button>
            </div>
            <div className="rounded-lg border border-border bg-card">
              {conversationEvents.length === 0 ? (
                <p className="p-4 text-xs text-muted-foreground">该对话无事件。</p>
              ) : (
                <div className="divide-y divide-border/60">
                  {conversationEvents.map((n, i) => (
                    <div key={n.id} className="flex items-center gap-2 px-3 py-1.5">
                      <span className="w-6 shrink-0 text-right font-mono text-[11px] text-muted-foreground">
                        {i + 1}
                      </span>
                      <span className="w-16 shrink-0 font-mono text-[11px] tabular-nums text-muted-foreground">
                        {formatTimestamp(n.timestamp)}
                      </span>
                      <DirectionBadge direction={n.direction ?? ""} />
                      <span
                        className="min-w-0 flex-1 truncate font-mono text-xs font-medium"
                        title={n.summary || n.id}
                      >
                        {nodeLabel(n)}
                      </span>
                      {n.is_push && (
                        <span className="shrink-0 rounded bg-primary/10 px-1 py-px text-[10px] font-medium uppercase text-primary">
                          push
                        </span>
                      )}
                      <span
                        className="shrink-0 max-w-[220px] truncate text-[11px] text-muted-foreground"
                        title={n.summary}
                      >
                        {n.summary}
                      </span>
                    </div>
                  ))}
                </div>
              )}
            </div>
          </div>
        ) : (
          <div>
            <div className="mb-2 flex items-center justify-between">
              <div className="flex items-center gap-2 text-sm font-semibold">
                <Network className="h-4 w-4 text-primary" />
                因果树
                <span className="text-xs font-normal text-muted-foreground">
                  按 TraceContext.CausationID 建父子关系，点击节点展开/收起
                </span>
              </div>
              {roots.length > 0 && (
                <button
                  type="button"
                  onClick={expandAll}
                  className="rounded-md border border-input px-2 py-1 text-xs hover:bg-muted/40"
                >
                  全部展开
                </button>
              )}
            </div>
            <div className="rounded-lg border border-border bg-card p-2">
              {isLoading ? (
                <div className="space-y-2 p-2">
                  {Array.from({ length: 6 }).map((_, i) => (
                    <Skeleton key={i} className="h-6 w-full rounded" />
                  ))}
                </div>
              ) : isError ? (
                <div
                  role="alert"
                  className="m-2 flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive"
                >
                  <AlertTriangle className="h-4 w-4 shrink-0" />
                  <span>加载时间线失败：{error?.message}</span>
                </div>
              ) : roots.length === 0 ? (
                <EmptyState
                  icon={<GitBranch className="h-5 w-5" />}
                  title="暂无时间线"
                  hint="该会话尚未产生可解码事件，或窗口被截断（可调大窗口重试）。"
                  className="h-48 justify-center"
                />
              ) : (
                roots.map((n) => (
                  <TimelineNodeView
                    key={n.id}
                    node={n}
                    depth={0}
                    expandedIds={expandedIds}
                    onToggle={toggleNode}
                  />
                ))
              )}
            </div>
          </div>
        )}
      </section>
    </div>
  );
}
