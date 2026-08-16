import { useMemo } from "react";
import { useEvidenceGraph } from "@/hooks/use-mcp";
import type { EvidenceNode, EvidenceEdge } from "@/types/evidence";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import { AlertTriangle, Network } from "lucide-react";

/** 节点 kind → 颜色（与 app 设计令牌对齐） */
const KIND_COLORS: Record<string, string> = {
  event: "#4f46e5",
  raw_packet: "#0284c7",
  state_change: "#059669",
  entity: "#d97706",
};
const DEFAULT_NODE_COLOR = "#6b7280";

const EDGE_COLORS: Record<string, string> = {
  response_to: "#4f46e5",
  decoded_from: "#0284c7",
  updates: "#059669",
  caused_by: "#d97706",
  correlated_with: "#9333ea",
  parameter_from: "#0d9488",
  possible_followup: "#94a3b8",
};

/** 基于 id 的确定性伪随机，保证布局稳定不抖动 */
function hashPos(id: string, i: number, w: number, h: number, rnd: () => number) {
  void id;
  const cols = Math.max(1, Math.floor(w / 90));
  const col = i % cols;
  const row = Math.floor(i / cols);
  const cellW = w / cols;
  const x = col * cellW + cellW / 2 + (rnd() - 0.5) * cellW * 0.7;
  const y = row * 80 + 50 + (rnd() - 0.5) * 40;
  return { x: Math.max(24, Math.min(w - 24, x)), y: Math.max(24, Math.min(h - 24, y)) };
}

function mulberry32(seed: number) {
  return function () {
    seed |= 0;
    seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const MAX_NODES = 160;
const W = 800;
const H = 480;

export function EvidenceGraph({
  sessionId,
  nodeKind,
  edgeType,
  minConfidence,
  rootNodeId,
  maxDepth,
  onSelectNode,
  selectedNodeId,
}: {
  sessionId: string | null;
  nodeKind: string;
  edgeType: string;
  minConfidence: number;
  rootNodeId: string;
  maxDepth: number;
  onSelectNode: (id: string) => void;
  selectedNodeId?: string;
}) {
  const { data, isLoading, isError, error } = useEvidenceGraph(sessionId, {
    nodeKind,
    edgeType,
    minConfidence,
    rootNodeId,
    maxDepth: rootNodeId ? maxDepth : 0,
    limit: 400,
  });

  const { positions, visibleNodes, visibleEdges } = useMemo(() => {
    const nodes = data?.nodes ?? [];
    const edges = data?.edges ?? [];
    const visibleNodes = nodes.slice(0, MAX_NODES);
    const nodeSet = new Set(visibleNodes.map((n) => n.id));
    const visibleEdges = edges.filter((e) => nodeSet.has(e.source) && nodeSet.has(e.target));
    const rnd = mulberry32(nodes.length + edges.length + 1);
    const positions = new Map<string, { x: number; y: number }>();
    visibleNodes.forEach((n, i) => positions.set(n.id, hashPos(n.id, i, W, H, rnd)));
    return { positions, visibleNodes, visibleEdges };
  }, [data]);

  if (isLoading) {
    return (
      <div className="p-4">
        <Skeleton className="h-[480px] w-full rounded-lg" />
      </div>
    );
  }
  if (isError) {
    return (
      <div className="p-4">
        <div
          role="alert"
          className="flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive"
        >
          <AlertTriangle className="h-4 w-4 shrink-0" />
          <span>证据图查询失败：{error?.message}</span>
        </div>
      </div>
    );
  }
  if (!data || visibleNodes.length === 0) {
    return (
      <EmptyState
        icon={<Network className="h-5 w-5" />}
        title="证据图为空"
        hint="该会话尚未构建证据图（需要解码出事件并生成实体/状态变更）。"
      />
    );
  }

  return (
    <div className="p-3">
      <svg viewBox={`0 0 ${W} ${H}`} width="100%" className="rounded-lg bg-secondary" style={{ maxHeight: 520 }}>
        {visibleEdges.map((e: EvidenceEdge, i) => {
          const a = positions.get(e.source);
          const b = positions.get(e.target);
          if (!a || !b) return null;
          const color = EDGE_COLORS[e.type] ?? "#94a3b8";
          return (
            <line
              key={`e-${i}`}
              x1={a.x}
              y1={a.y}
              x2={b.x}
              y2={b.y}
              stroke={color}
              strokeWidth={1}
              opacity={0.25 + Math.min(1, e.confidence) * 0.5}
            />
          );
        })}
        {visibleNodes.map((n: EvidenceNode) => {
          const p = positions.get(n.id)!;
          const color = KIND_COLORS[n.kind] ?? DEFAULT_NODE_COLOR;
          const selected = n.id === selectedNodeId;
          return (
            <g
              key={n.id}
              className="cursor-pointer"
              onClick={() => onSelectNode(n.id)}
            >
              <circle
                cx={p.x}
                cy={p.y}
                r={selected ? 10 : 7}
                fill={color}
                stroke={selected ? "#15171e" : "none"}
                strokeWidth={selected ? 2 : 0}
              >
                <title>{`${n.kind} · ${n.id}`}</title>
              </circle>
            </g>
          );
        })}
      </svg>
      <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
        {Object.entries(KIND_COLORS).map(([k, c]) => (
          <span key={k} className="inline-flex items-center gap-1.5">
            <span className="inline-block h-2.5 w-2.5 rounded-full" style={{ background: c }} />
            {k}
          </span>
        ))}
        <span className="ml-auto">
          显示 {visibleNodes.length} / 共 {data?.nodes.length ?? 0} 节点
        </span>
      </div>
    </div>
  );
}
