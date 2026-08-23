import { useMemo, useState } from "react";
import { useSessionTimeline } from "@/hooks/use-mcp";
import type { TimelineNode, SemanticStatus } from "@/types/timeline";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import {
  GitBranch,
  Network,
  AlertTriangle,
  ArrowRight,
  ArrowLeft,
  ArrowDown,
  Bell,
  Clock,
  X,
  CheckCircle2,
  OctagonX,
  Loader2,
} from "lucide-react";

// ─── 语义状态（固定映射，前端统一决定颜色，不在各处猜） ──────

const STATUS_META: Record<
  SemanticStatus,
  { label: string; dot: string; text: string }
> = {
  normal: { label: "Normal", dot: "bg-slate-400", text: "text-slate-500" },
  success: { label: "Success", dot: "bg-emerald-500", text: "text-emerald-600 dark:text-emerald-400" },
  warning: { label: "Timeout", dot: "bg-amber-500", text: "text-amber-600 dark:text-amber-400" },
  error: { label: "Error", dot: "bg-red-500", text: "text-red-600 dark:text-red-400" },
  pending: { label: "Pending", dot: "bg-blue-500", text: "text-blue-600 dark:text-blue-400" },
  unknown: { label: "Unknown", dot: "bg-slate-300", text: "text-slate-500" },
};

// 消息语义名：优雅回退 proto.message → msg_name → type → (unknown)
function semanticName(node: TimelineNode): string {
  return node.proto?.message || node.msg_name || node.type || "(unknown)";
}

// 该节点是否有可用的通信语义（无语义则降级为普通 JSON Event）
function hasSemantic(node: TimelineNode): boolean {
  const p = node.proto;
  return !!p && !!(p.message || p.role || p.delivery);
}

// ─── 方向渲染 ──────────────────────────────────────────────

function DirectionBadge({ direction, semantic }: { direction: string; semantic?: boolean }) {
  if (!semantic) {
    return (
      <span className="inline-flex shrink-0 items-center rounded bg-muted px-1.5 py-px text-[10px] text-muted-foreground">
        ?
      </span>
    );
  }
  switch (direction) {
    case "client_to_server":
      return (
        <span className="inline-flex shrink-0 items-center gap-0.5 rounded-full bg-blue-50 px-1.5 py-px text-[11px] font-medium text-blue-700 dark:bg-blue-950 dark:text-blue-300">
          <ArrowRight className="h-3 w-3" />
          C→S
        </span>
      );
    case "server_to_client":
      return (
        <span className="inline-flex shrink-0 items-center gap-0.5 rounded-full bg-emerald-50 px-1.5 py-px text-[11px] font-medium text-emerald-700 dark:bg-emerald-950 dark:text-emerald-300">
          <ArrowLeft className="h-3 w-3" />
          S→C
        </span>
      );
    default:
      return (
        <span className="inline-flex shrink-0 items-center rounded bg-muted px-1.5 py-px text-[10px] text-muted-foreground">
          ?
        </span>
      );
  }
}

// ─── 时间格式化 ────────────────────────────────────────────

function formatTime(isoStr: string): string {
  try {
    return new Date(isoStr).toLocaleString("zh-CN", {
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hour12: false,
    });
  } catch {
    return isoStr;
  }
}

function formatTimestampFull(isoStr: string): string {
  try {
    const ms = new Date(isoStr).getMilliseconds();
    return (
      formatTime(isoStr) +
      "." +
      String(ms).padStart(3, "0")
    );
  } catch {
    return isoStr;
  }
}

function tsMs(isoStr: string): number {
  return new Date(isoStr).getTime();
}

// ─── 数据派生：拍平时序 + RPC 配对 + 状态 ──────────────────

interface Enriched {
  node: TimelineNode;
  status: SemanticStatus;
}

// 把因果树拍平为时间升序列表（语义判断保存在节点上）
function flattenNodes(roots: TimelineNode[]): TimelineNode[] {
  const out: TimelineNode[] = [];
  const walk = (nodes: TimelineNode[] | undefined) => {
    for (const n of nodes ?? []) {
      out.push(n);
      walk(n.children);
    }
  };
  walk(roots);
  return out.sort((a, b) => a.timestamp.localeCompare(b.timestamp));
}

/** 计算单事件状态（不考虑配对时，用于 Flow 与检索）。 */
function statusOf(n: TimelineNode, hasResponse?: boolean): SemanticStatus {
  if (!hasSemantic(n)) return "unknown";
  const role = n.proto?.role;
  if (role === "push") return "normal";
  if (role === "request") return hasResponse ? "pending" : "warning";
  if (role === "response") return n.proto?.error?.failed ? "error" : "success";
  return "unknown";
}

type TimelineEntry =
  | { kind: "rpc"; request: Enriched; response: Enriched; latencyMs?: number }
  | { kind: "single"; item: Enriched };

function buildEntries(roots: TimelineNode[]): TimelineEntry[] {
  const nodes = flattenNodes(roots);
  const used = new Set<string>();
  const entries: TimelineEntry[] = [];

  for (let i = 0; i < nodes.length; i++) {
    const n = nodes[i]!;
    if (used.has(n.id)) continue;
    const role = n.proto?.role;

    // 请求：向后找同 correlation 的下一个 response 配对
    if (role === "request" && hasSemantic(n)) {
      let matched: TimelineNode | null = null;
      for (let j = i + 1; j < nodes.length; j++) {
        const candidate = nodes[j]!;
        if (used.has(candidate.id)) continue;
        const isResponse = candidate.proto?.role === "response";
        const sameCorr =
          n.correlation_id && candidate.correlation_id === n.correlation_id;
        if (isResponse && sameCorr) {
          matched = candidate;
          break;
        }
      }
      if (matched) {
        used.add(n.id);
        used.add(matched.id);
        const req: Enriched = { node: n, status: statusOf(n, true) };
        const resp: Enriched = { node: matched, status: statusOf(matched) };
        const latencyMs = Math.max(0, tsMs(matched.timestamp) - tsMs(n.timestamp));
        entries.push({ kind: "rpc", request: req, response: resp, latencyMs });
        continue;
      }
      // 无响应：视为超时/未决（warning，非 error）
      entries.push({ kind: "single", item: { node: n, status: "warning" } });
      continue;
    }

    entries.push({ kind: "single", item: { node: n, status: statusOf(n) } });
  }
  return entries;
}

// ─── Flow：按 correlation 分组 Client/Server 两侧 ──────────

interface FlowNode {
  node: TimelineNode;
  status: SemanticStatus;
}

interface FlowGroup {
  correlationId: string;
  left: FlowNode[]; // C→S（请求侧）
  right: FlowNode[]; // S→C（响应侧）
}

function buildFlow(roots: TimelineNode[]): FlowGroup[] {
  const nodes = flattenNodes(roots);
  const groups = new Map<string, FlowNode[]>();
  const singles: FlowNode[] = [];
  for (const n of nodes) {
    const item: FlowNode = { node: n, status: statusOf(n) };
    if (n.correlation_id) {
      const arr = groups.get(n.correlation_id) ?? [];
      arr.push(item);
      groups.set(n.correlation_id, arr);
    } else {
      singles.push(item);
    }
  }
  const out: FlowGroup[] = [];
  for (const [correlationId, items] of groups) {
    const left: FlowNode[] = [];
    const right: FlowNode[] = [];
    for (const it of items) {
      if (it.node.direction === "client_to_server" || it.node.proto?.role === "request") {
        left.push(it);
      } else if (
        it.node.direction === "server_to_client" ||
        it.node.proto?.role === "response"
      ) {
        right.push(it);
      } else {
        // push / 无语义：放入右侧（Server 推送）
        right.push(it);
      }
    }
    out.push({ correlationId, left, right });
  }
  // 独立（无 correlation push）归为一个组
  if (singles.length > 0) {
    out.push({ correlationId: "", left: [], right: singles });
  }
  return out;
}

// ─── Message Detail 侧栏 ───────────────────────────────────

function MessageDetail({
  node,
  onClose,
}: {
  node: TimelineNode;
  onClose: () => void;
}) {
  const [tab, setTab] = useState<"semantic" | "json">("semantic");
  const semantic = hasSemantic(node);
  const status = statusOf(node);
  const st = STATUS_META[status];

  // 语义侧展示辅助字段
  const role = node.proto?.role;
  const corr = node.proto?.correlation;
  const err = node.proto?.error;

  // 原始 JSON（缩进美化）；没有干净 json 时回退 summary
  const raw = useMemo(() => {
    const src = node.json ?? node.summary ?? "";
    if (!src) return "";
    try {
      return JSON.stringify(JSON.parse(src), null, 2);
    } catch {
      return src;
    }
  }, [node.json, node.summary]);

  return (
    <div className="flex h-full w-full flex-col border-l border-border bg-card">
      {/* 头部 */}
      <div className="flex items-center justify-between gap-2 border-b border-border px-3 py-2">
        <div className="min-w-0">
          <p className="flex items-center gap-1.5 truncate font-mono text-sm font-semibold">
            <span className={`h-2 w-2 shrink-0 rounded-full ${st.dot}`} />
            {semanticName(node)}
          </p>
          <p className="truncate font-mono text-[11px] text-muted-foreground">
            {formatTimestampFull(node.timestamp)}
          </p>
        </div>
        <button
          type="button"
          onClick={onClose}
          aria-label="关闭消息详情"
          className="inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-md text-muted-foreground hover:bg-muted/60 hover:text-foreground"
        >
          <X className="h-4 w-4" />
        </button>
      </div>

      {/* Semantic / Raw JSON 双 Tab：不隐藏原始 JSON */}
      <div className="flex items-center gap-1 border-b border-border px-3 py-1.5">
        {(["semantic", "json"] as const).map((t) => (
          <button
            key={t}
            type="button"
            onClick={() => setTab(t)}
            className={
              "rounded-md px-2.5 py-1 text-xs font-medium transition-colors " +
              (tab === t
                ? "bg-muted text-foreground"
                : "text-muted-foreground hover:text-foreground")
            }
          >
            {t === "semantic" ? "Semantic" : "JSON"}
          </button>
        ))}
        {!semantic && (
          <span className="ml-auto rounded bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
            无语义语义 · 已降级
          </span>
        )}
      </div>

      <div className="min-h-0 flex-1 overflow-auto gta-scroll p-3">
        {tab === "semantic" ? (
          <div className="space-y-3">
            {/* 状态 */}
            <DetailRow label="状态">
              <span className={`inline-flex items-center gap-1 text-xs font-medium ${st.text}`}>
                {status === "error" && <OctagonX className="h-3.5 w-3.5" />}
                {status === "warning" && <Clock className="h-3.5 w-3.5" />}
                {status === "success" && <CheckCircle2 className="h-3.5 w-3.5" />}
                {status === "pending" && <Loader2 className="h-3.5 w-3.5" />}
                {st.label}
              </span>
            </DetailRow>

            {semantic ? (
              <>
                <DetailRow label="Message">{node.proto?.message ?? semanticName(node)}</DetailRow>
                <DetailRow label="Role">
                  {role === "request" ? "Request" : role === "response" ? "Response" : role === "push" ? "Push" : "Unknown"}
                </DetailRow>
                {node.proto?.delivery && <DetailRow label="Delivery">{node.proto.delivery}</DetailRow>}
                {corr && corr.value && (
                  <DetailRow label="Correlation">
                    <span className="font-mono">{corr.key}={corr.value}</span>
                  </DetailRow>
                )}
                <DetailRow label="Direction">
                  {node.direction === "client_to_server"
                    ? "Client → Server"
                    : node.direction === "server_to_client"
                      ? "Server → Client"
                      : "Unknown"}
                </DetailRow>
              </>
            ) : (
              <DetailRow label="Message">
                <span className="text-muted-foreground">Unknown Message</span>
              </DetailRow>
            )}

            {/* 错误细节 */}
            {err?.failed && (
              <div className="rounded-md border border-red-300/40 bg-red-50 px-2.5 py-2 text-xs text-red-700 dark:bg-red-950/40 dark:text-red-300">
                <div className="mb-0.5 font-semibold">FAILED</div>
                {err.code && <div className="font-mono">Error Code: {err.code}</div>}
              </div>
            )}

            {/* 原始事件元信息 */}
            <div className="rounded-md border border-border bg-muted/30 px-2.5 py-2 text-[11px] text-muted-foreground">
              <div className="mb-1 font-medium text-foreground">Event</div>
              <Row label="ID">{node.id}</Row>
              {node.correlation_id && <Row label="Correlation ID">{node.correlation_id}</Row>}
              <Row label="Schema">{node.schema_id || "—"}</Row>
            </div>
          </div>
        ) : (
          <pre className="whitespace-pre-wrap break-words rounded-md bg-background p-3 font-mono text-xs leading-relaxed gta-scroll">
            {raw || "{}"}
          </pre>
        )}
      </div>
    </div>
  );
}

function DetailRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-start gap-2 text-xs">
      <span className="w-20 shrink-0 text-muted-foreground">{label}</span>
      <span className="min-w-0 flex-1 font-medium">{children}</span>
    </div>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex gap-2">
      <span className="w-24 shrink-0 text-muted-foreground">{label}</span>
      <span className="min-w-0 flex-1 truncate font-mono" title={String(children)}>
        {children}
      </span>
    </div>
  );
}

// ─── Trail/Flow 图（简版 RPC 连桥，Flow 视图复用）──────────

// ─── 主面板 ──────────────────────────────────────────────

export function TimelinePanel({ sessionId }: { sessionId: string | null }) {
  const [limit, setLimit] = useState(500);
  const [view, setView] = useState<"timeline" | "flow">("timeline");
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const { data, isLoading, isError, error } = useSessionTimeline(sessionId, { limit });

  const roots = data?.roots ?? [];
  const selectedNode = useMemo(() => {
    if (!selectedId) return null;
    const nodes = flattenNodes(roots);
    return nodes.find((n) => n.id === selectedId) ?? null;
  }, [roots, selectedId]);

  const entries = useMemo(() => buildEntries(roots), [roots]);
  const flow = useMemo(() => buildFlow(roots), [roots]);

  const stats = useMemo(() => {
    const counts: Record<SemanticStatus, number> = {
      normal: 0,
      success: 0,
      warning: 0,
      error: 0,
      pending: 0,
      unknown: 0,
    };
    for (const e of flattenNodes(roots)) {
      counts[statusOf(e)]++;
    }
    return counts;
  }, [roots]);

  if (!sessionId) {
    return (
      <EmptyState
        icon={<Network className="h-5 w-5" />}
        title="未选择会话"
        hint="在左侧选择一个会话后，这里会展示整次抓包的通信时序（Timeline）、通信流（Flow）与单条消息详情。"
      />
    );
  }

  return (
    <div className="flex h-full min-h-0 overflow-hidden">
      {/* 左侧主区 */}
      <div className="flex min-w-0 flex-1 flex-col">
        {/* 头部：视图切换 + 会话上下文 */}
        <div className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-2.5">
          <div className="flex items-center gap-1 rounded-lg bg-muted p-1">
            {(
              [
                { id: "timeline", label: "Timeline" },
                { id: "flow", label: "Flow" },
              ] as const
            ).map((t) => (
              <button
                key={t.id}
                type="button"
                onClick={() => setView(t.id)}
                className={
                  "rounded-md px-3 py-1 text-xs font-medium transition-colors " +
                  (view === t.id
                    ? "bg-background text-foreground shadow-sm"
                    : "text-muted-foreground hover:text-foreground")
                }
              >
                {t.label}
              </button>
            ))}
          </div>

          {data && (
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-muted-foreground">
              <span>
                事件 <span className="font-mono tabular-nums">{data.event_count}</span>
              </span>
              {data.plugin && (
                <span className="rounded bg-muted px-1.5 py-0.5 font-mono">{data.plugin}</span>
              )}
              {data.status && <span className="rounded bg-muted px-1.5 py-0.5">{data.status}</span>}
              <label className="ml-auto flex items-center gap-1">
                窗口
                <select
                  className="h-6 rounded-md border border-input bg-background px-1 text-[11px] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
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
        </div>

        {/* 状态图例 + 不确定性 */}
        <div className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-1.5">
          {(Object.keys(STATUS_META) as SemanticStatus[]).map((s) => (
            <span
              key={s}
              className="inline-flex items-center gap-1 text-[10px] text-muted-foreground"
            >
              <span className={`h-1.5 w-1.5 rounded-full ${STATUS_META[s].dot}`} />
              {STATUS_META[s].label}
              <span className="font-mono tabular-nums">{stats[s]}</span>
            </span>
          ))}
        </div>

        {(data?.uncertainties ?? []).length > 0 && (
          <div className="border-b border-border px-4 py-1.5">
            {(data?.uncertainties ?? []).map((u, i) => (
              <div
                key={i}
                className="flex items-center gap-1.5 text-[11px] text-amber-700 dark:text-amber-300"
              >
                <AlertTriangle className="h-3 w-3 shrink-0" />
                <span>{u}</span>
              </div>
            ))}
          </div>
        )}

        {/* 内容 */}
        <div className="min-h-0 flex-1 overflow-auto gta-scroll p-4">
          {isLoading ? (
            <div className="space-y-2">
              {Array.from({ length: 6 }).map((_, i) => (
                <Skeleton key={i} className="h-10 w-full rounded-lg" />
              ))}
            </div>
          ) : isError ? (
            <div
              role="alert"
              className="flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive"
            >
              <AlertTriangle className="h-4 w-4 shrink-0" />
              <span>加载时间线失败：{error?.message}</span>
            </div>
          ) : roots.length === 0 ? (
            <EmptyState
              icon={<GitBranch className="h-5 w-5" />}
              title="暂无通信"
              hint="该会话尚未产生可解码事件，或窗口被截断（可调大窗口重试）。"
              className="h-64 justify-center"
            />
          ) : view === "timeline" ? (
            <TimelineList entries={entries} selectedId={selectedId} onSelect={setSelectedId} />
          ) : (
            <FlowView flow={flow} selectedId={selectedId} onSelect={setSelectedId} />
          )}
        </div>
      </div>

      {/* 右侧：Message Detail 侧栏 */}
      {selectedNode && (
        <div className="w-[380px] shrink-0" style={{ minHeight: 0 }}>
          <MessageDetail node={selectedNode} onClose={() => setSelectedId(null)} />
        </div>
      )}
    </div>
  );
}

// ─── Timeline 视图（默认）────────────────────────────────

function TimelineList({
  entries,
  selectedId,
  onSelect,
}: {
  entries: TimelineEntry[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  return (
    <div className="space-y-2">
      {entries.map((entry) =>
        entry.kind === "rpc" ? (
          <RpcCall key={entry.request.node.id} entry={entry} selectedId={selectedId} onSelect={onSelect} />
        ) : (
          <SingleRow key={entry.item.node.id} item={entry.item} selectedId={selectedId} onSelect={onSelect} />
        ),
      )}
    </div>
  );
}

// 一条普通消息（Push / 无语义 Unknown / 未配对请求）
function SingleRow({
  item,
  selectedId,
  onSelect,
}: {
  item: { node: TimelineNode; status: SemanticStatus };
  onSelect: (id: string) => void;
  selectedId: string | null;
}) {
  const { node, status } = item;
  const isPush = node.proto?.role === "push";
  const semantic = hasSemantic(node);
  const st = STATUS_META[status];

  return (
    <button
      type="button"
      onClick={() => onSelect(node.id)}
      className={
        "flex w-full items-center gap-2.5 rounded-lg border px-3 py-2 text-left transition-colors " +
        (selectedId === node.id
          ? "border-primary bg-primary/5"
          : "border-border bg-card hover:bg-muted/30")
      }
    >
      <span className="w-16 shrink-0 font-mono text-[11px] tabular-nums text-muted-foreground">
        {formatTime(node.timestamp)}
      </span>
      <span className={`h-2 w-2 shrink-0 rounded-full ${st.dot}`} title={st.label} />
      {isPush && <Bell className="h-3.5 w-3.5 shrink-0 text-primary" />}
      <span className="min-w-0 flex-1 truncate font-mono text-xs font-medium text-foreground">
        {semanticName(node)}
      </span>
      <span className="hidden w-14 shrink-0 sm:block">
        <DirectionBadge direction={isPush ? "server_to_client" : node.direction ?? ""} semantic={semantic} />
      </span>
      {isPush && <PushTag />}
      {status === "warning" && (
        <span className="inline-flex shrink-0 items-center gap-1 rounded bg-amber-100 px-1.5 py-px text-[10px] font-medium text-amber-700 dark:bg-amber-900/40 dark:text-amber-300">
          <Clock className="h-3 w-3" />
          {item.node.proto?.role === "request" ? "无响应" : "Timeout"}
        </span>
      )}
    </button>
  );
}

// Request → Response 配对卡片
function RpcCall({
  entry,
  selectedId,
  onSelect,
}: {
  entry: Extract<TimelineEntry, { kind: "rpc" }>;
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  const req = entry.request.node;
  const resp = entry.response.node;
  const respStatus = entry.response.status;
  const respErr = resp.proto?.error;
  const latency = entry.latencyMs;

  return (
    <div
      className={
        "overflow-hidden rounded-lg border bg-card " +
        (selectedId === req.id || selectedId === resp.id ? "border-primary" : "border-border")
      }
    >
      {/* 请求行 */}
      <button
        type="button"
        onClick={() => onSelect(req.id)}
        className="flex w-full items-center gap-2.5 px-3 py-1.5 text-left transition-colors hover:bg-muted/30"
      >
        <span className="w-16 shrink-0 font-mono text-[11px] tabular-nums text-muted-foreground">
          {formatTime(req.timestamp)}
        </span>
        <span className={`h-2 w-2 shrink-0 rounded-full ${STATUS_META.pending.dot}`} />
        <span className="min-w-0 flex-1 truncate font-mono text-xs font-semibold text-foreground">
          {semanticName(req)}
        </span>
        <span className="hidden shrink-0 sm:block">
          <DirectionBadge direction="client_to_server" semantic />
        </span>
        {req.proto?.correlation?.value && (
          <span className="hidden shrink-0 font-mono text-[10px] text-muted-foreground md:inline">
            {req.proto.correlation.key}={req.proto.correlation.value}
          </span>
        )}
      </button>

      {/* 连接线与耗时 */}
      <div className="flex items-center gap-1.5 px-3 py-0.5 pl-[104px]">
        <ArrowDown className="h-3 w-3 shrink-0 text-muted-foreground/60" />
        <span className="font-mono text-[10px] tabular-nums text-muted-foreground">
          {latency !== undefined ? `${latency}ms` : "—"}
        </span>
      </div>

      {/* 响应行 */}
      <button
        type="button"
        onClick={() => onSelect(resp.id)}
        className="flex w-full items-center gap-2.5 px-3 py-1.5 text-left transition-colors hover:bg-muted/30"
      >
        <span className="w-16 shrink-0 font-mono text-[11px] tabular-nums text-muted-foreground">
          {formatTime(resp.timestamp)}
        </span>
        <span className={`h-2 w-2 shrink-0 rounded-full ${STATUS_META[respStatus].dot}`} />
        <span className="min-w-0 flex-1 truncate font-mono text-xs font-semibold text-foreground">
          {semanticName(resp)}
        </span>
        <span className="hidden shrink-0 sm:block">
          <DirectionBadge direction="server_to_client" semantic />
        </span>
        {respStatus === "success" &&
          respErr?.code !== undefined && (
            <span className="inline-flex shrink-0 items-center gap-1 rounded bg-emerald-100 px-1.5 py-px text-[10px] font-medium text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300">
              {respErr?.failed ? `Error ${respErr.code}` : `OK${respErr.code ? " " + respErr.code : ""}`}
            </span>
          )}
        {respStatus === "error" && (
          <span className="inline-flex shrink-0 items-center gap-1 rounded bg-red-100 px-1.5 py-px text-[10px] font-medium text-red-700 dark:bg-red-900/40 dark:text-red-300">
            <OctagonX className="h-3 w-3" />
            Error {respErr?.code ?? ""}
          </span>
        )}
      </button>
    </div>
  );
}

function PushTag() {
  return (
    <span className="inline-flex shrink-0 items-center gap-0.5 rounded bg-primary/10 px-1.5 py-px text-[10px] font-medium uppercase text-primary">
      <Bell className="h-3 w-3" />
      push
    </span>
  );
}

// ─── Flow 视图 ───────────────────────────────────────────

function FlowView({
  flow,
  selectedId,
  onSelect,
}: {
  flow: FlowGroup[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  return (
    <div className="space-y-3">
      {flow.length === 0 && (
        <EmptyState icon={<Network className="h-5 w-5" />} title="无通信流" hint="尚无带 direction/correlation 的事件。" />
      )}
      {flow.map((g) => {
        const label =
          g.correlationId !== ""
            ? `Correlation #${g.correlationId.slice(0, 10)}`
            : "独立消息";
        return (
          <div key={g.correlationId || "single"} className="rounded-lg border border-border bg-card">
            <div className="border-b border-border px-3 py-1.5 text-[11px] font-medium text-muted-foreground">
              {label}
            </div>
            {/* 两栏：Client | Server */}
            <div className="grid grid-cols-[1fr_auto_1fr] gap-2 p-3">
              <div className="space-y-1.5">
                <div className="mb-1 text-center text-[10px] uppercase tracking-wide text-muted-foreground">
                  Client
                </div>
                {g.left.length === 0 && <div className="text-center text-[11px] text-muted-foreground/50">—</div>}
                {g.left.map((it) => (
                  <FlowChip key={it.node.id} item={it} selectedId={selectedId} onSelect={onSelect} side="right" />
                ))}
              </div>

              {/* 中间箭头区 */}
              <div className="flex flex-col items-center justify-center px-1">
                {g.left.length > 0 && g.right.length > 0 && (
                  <div className="flex flex-col items-center gap-1">
                    <ArrowRight className="h-4 w-4 text-muted-foreground" />
                    <ArrowLeft className="h-4 w-4 text-muted-foreground" />
                  </div>
                )}
                {g.left.length === 0 && g.right.length > 0 && (
                  <ArrowLeft className="h-4 w-4 text-primary" />
                )}
              </div>

              <div className="space-y-1.5">
                <div className="mb-1 text-center text-[10px] uppercase tracking-wide text-muted-foreground">
                  Server
                </div>
                {g.right.length === 0 && <div className="text-center text-[11px] text-muted-foreground/50">—</div>}
                {g.right.map((it) => (
                  <FlowChip key={it.node.id} item={it} selectedId={selectedId} onSelect={onSelect} side="left" />
                ))}
              </div>
            </div>
          </div>
        );
      })}
    </div>
  );
}

function FlowChip({
  item,
  selectedId,
  onSelect,
  side,
}: {
  item: FlowNode;
  selectedId: string | null;
  onSelect: (id: string) => void;
  side: "left" | "right";
}) {
  const st = STATUS_META[item.status];
  const arrow = side === "left" ? <ArrowLeft className="h-3 w-3" /> : <ArrowRight className="h-3 w-3" />;
  return (
    <button
      type="button"
      onClick={() => onSelect(item.node.id)}
      className={
        "flex w-full items-center gap-1.5 rounded-md border px-2 py-1 text-left font-mono text-[11px] transition-colors " +
        (selectedId === item.node.id
          ? "border-primary bg-primary/5 text-foreground"
          : "border-border bg-background text-muted-foreground hover:bg-muted/30")
      }
      style={{ flexDirection: side === "right" ? "row-reverse" : "row" }}
      title={item.node.id}
    >
      {arrow}
      <span className={`min-w-0 flex-1 truncate ${st.text} font-medium`}>
        {semanticName(item.node)}
      </span>
    </button>
  );
}