import { useEffect, useState } from "react";
import { useTraceEventChain } from "@/hooks/use-mcp";
import type { TraceHop } from "@/types/evidence";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import { AlertTriangle, ArrowDownUp, GitBranch } from "lucide-react";

function HopList({ title, hops, tone }: { title: string; hops: TraceHop[]; tone: "up" | "down" }) {
  return (
    <div className="min-w-0 flex-1">
      <div className="mb-2 text-xs font-medium text-muted-foreground">
        {title}（{hops.length}）
      </div>
      <div className="space-y-1.5">
        {hops.length === 0 && <p className="text-xs text-muted-foreground/80">无</p>}
        {hops.map((h, i) => (
          <div
            key={`${h.node_id}-${i}`}
            className="rounded-md border border-border bg-card px-2 py-1.5 text-xs"
          >
            <div className="flex items-center gap-2">
              <span
                className={
                  "inline-flex h-5 min-w-5 items-center justify-center rounded px-1 text-[11px] font-medium " +
                  (tone === "up" ? "bg-info/10 text-info" : "bg-primary-muted text-accent-foreground")
                }
              >
                d{h.depth}
              </span>
              <span className="truncate font-mono" title={h.node_id}>
                {h.node_id}
              </span>
            </div>
            <div className="mt-1 flex items-center gap-2 text-[11px] text-muted-foreground">
              <span className="rounded bg-muted px-1.5 py-0.5">{h.edge_type}</span>
              <span>置信 {h.confidence.toFixed(2)}</span>
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

export function TracePanel({
  sessionId,
  initialNodeId,
}: {
  sessionId: string | null;
  initialNodeId?: string;
}) {
  const [nodeId, setNodeId] = useState(initialNodeId ?? "");
  const [eventId, setEventId] = useState("");
  const [maxDepth, setMaxDepth] = useState(5);
  const [minConfidence, setMinConfidence] = useState(0.5);

  // 从证据图点选节点时同步到追踪输入
  useEffect(() => {
    if (initialNodeId) setNodeId(initialNodeId);
  }, [initialNodeId]);

  const trace = useTraceEventChain(sessionId, {
    nodeId: nodeId || undefined,
    eventId: eventId || undefined,
    maxDepth,
    minConfidence,
  });

  const target = nodeId || eventId;

  return (
    <div className="p-3">
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <input
          value={eventId}
          onChange={(e) => setEventId(e.target.value)}
          placeholder="event_id"
          className="h-8 w-40 rounded-md border border-input bg-background px-2 font-mono text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
        />
        <input
          value={nodeId}
          onChange={(e) => setNodeId(e.target.value)}
          placeholder="node_id（证据图点选）"
          className="h-8 w-48 rounded-md border border-input bg-background px-2 font-mono text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
        />
        <label className="flex items-center gap-1 text-xs text-muted-foreground">
          深度
          <input
            type="number"
            min={1}
            max={10}
            value={maxDepth}
            onChange={(e) => setMaxDepth(Number(e.target.value) || 5)}
            className="h-8 w-14 rounded-md border border-input bg-background px-2 text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
          />
        </label>
        <label className="flex items-center gap-1 text-xs text-muted-foreground">
          最小置信
          <input
            type="number"
            min={0}
            max={1}
            step={0.05}
            value={minConfidence}
            onChange={(e) => setMinConfidence(Number(e.target.value) || 0)}
            className="h-8 w-16 rounded-md border border-input bg-background px-2 text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
          />
        </label>
      </div>

      {!target ? (
        <EmptyState
          icon={<ArrowDownUp className="h-5 w-5" />}
          title="选择追踪起点"
          hint="在上方输入 event_id / node_id，或在证据图中点击一个节点以追踪其上下游因果链。"
        />
      ) : trace.isLoading ? (
        <div className="space-y-2">
          <Skeleton className="h-32 w-full rounded-lg" />
          <Skeleton className="h-32 w-full rounded-lg" />
        </div>
      ) : trace.isError ? (
        <div
          role="alert"
          className="flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive"
        >
          <AlertTriangle className="h-4 w-4 shrink-0" />
          <span>追踪失败：{trace.error?.message}</span>
        </div>
      ) : trace.data ? (
        <div>
          <div className="mb-2 flex items-center gap-2 text-xs text-muted-foreground">
            <GitBranch className="h-3.5 w-3.5" />
            起点 <span className="font-mono">{trace.data.start_node_id}</span>
          </div>
          <div className="flex gap-3">
            <HopList title="上游（谁导致）" hops={trace.data.upstream} tone="up" />
            <HopList title="下游（导致什么）" hops={trace.data.downstream} tone="down" />
          </div>
        </div>
      ) : null}
    </div>
  );
}
