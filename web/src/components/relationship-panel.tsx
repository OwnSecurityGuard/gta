import { useState } from "react";
import { EvidenceGraph } from "@/components/evidence-graph";
import { TracePanel } from "@/components/trace-panel";
import { LinkRuleTable } from "@/components/link-rule-table";
import { EmptyState } from "@/components/ui/empty-state";
import { BarChart3, Link2, Network, GitBranch } from "lucide-react";

/** 与 evidence-graph 颜色映射保持一致的候选取值（"全部" 用空串） */
const NODE_KINDS = ["", "event", "raw_packet", "state_change", "entity"];
const EDGE_TYPES = [
  "",
  "response_to",
  "decoded_from",
  "updates",
  "caused_by",
  "correlated_with",
  "parameter_from",
  "possible_followup",
];

function GraphControls({
  nodeKind,
  edgeType,
  minConfidence,
  rootNodeId,
  maxDepth,
  onNodeKind,
  onEdgeType,
  onMinConfidence,
  onRootNodeId,
  onMaxDepth,
}: {
  nodeKind: string;
  edgeType: string;
  minConfidence: number;
  rootNodeId: string;
  maxDepth: number;
  onNodeKind: (v: string) => void;
  onEdgeType: (v: string) => void;
  onMinConfidence: (v: number) => void;
  onRootNodeId: (v: string) => void;
  onMaxDepth: (v: number) => void;
}) {
  const selectCls =
    "h-8 rounded-md border border-input bg-background px-2 text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30";
  return (
    <div className="mb-2 flex flex-wrap items-center gap-2">
      <label className="flex items-center gap-1 text-xs text-muted-foreground">
        节点
        <select value={nodeKind} onChange={(e) => onNodeKind(e.target.value)} className={selectCls}>
          {NODE_KINDS.map((k) => (
            <option key={k} value={k}>
              {k === "" ? "全部" : k}
            </option>
          ))}
        </select>
      </label>
      <label className="flex items-center gap-1 text-xs text-muted-foreground">
        边
        <select value={edgeType} onChange={(e) => onEdgeType(e.target.value)} className={selectCls}>
          {EDGE_TYPES.map((t) => (
            <option key={t} value={t}>
              {t === "" ? "全部" : t}
            </option>
          ))}
        </select>
      </label>
      <label className="flex items-center gap-1 text-xs text-muted-foreground">
        最小置信
        <input
          type="number"
          min={0}
          max={1}
          step={0.05}
          value={minConfidence}
          onChange={(e) => onMinConfidence(Number(e.target.value) || 0)}
          className="h-8 w-16 rounded-md border border-input bg-background px-2 text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
        />
      </label>
      {rootNodeId && (
        <label className="flex items-center gap-1 text-xs text-muted-foreground">
          根节点
          <input
            value={rootNodeId}
            onChange={(e) => onRootNodeId(e.target.value)}
            placeholder="root_node_id（留空则全图）"
            className="h-8 w-48 rounded-md border border-input bg-background px-2 font-mono text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
          />
        </label>
      )}
      <label className="flex items-center gap-1 text-xs text-muted-foreground">
        深度
        <input
          type="number"
          min={1}
          max={6}
          value={maxDepth}
          onChange={(e) => onMaxDepth(Number(e.target.value) || 3)}
          className="h-8 w-14 rounded-md border border-input bg-background px-2 text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
        />
      </label>
    </div>
  );
}

function SectionHeader({ icon, title, hint }: { icon: React.ReactNode; title: string; hint?: string }) {
  return (
    <div className="mb-2 flex items-center gap-2 text-sm font-semibold">
      {icon}
      <span className="text-foreground">{title}</span>
      {hint && <span className="text-xs font-normal text-muted-foreground">{hint}</span>}
    </div>
  );
}

export function RelationshipPanel({ sessionId }: { sessionId: string | null }) {
  const [nodeKind, setNodeKind] = useState("");
  const [edgeType, setEdgeType] = useState("");
  const [minConfidence, setMinConfidence] = useState(0);
  const [rootNodeId, setRootNodeId] = useState("");
  const [maxDepth, setMaxDepth] = useState(3);
  const [selectedNodeId, setSelectedNodeId] = useState<string | undefined>(undefined);

  return (
    <div className="h-full overflow-auto p-4 gta-scroll">
      {!sessionId ? (
        <EmptyState
          icon={<Link2 className="h-5 w-5" />}
          title="未选择会话"
          hint="在左侧选择一个会话后，这里会展示语义证据图、因果链追踪与链路规则建议。"
        />
      ) : (
        <div className="space-y-5">
          {/* 语义证据图 */}
          <section>
            <SectionHeader
              icon={<Network className="h-4 w-4 text-primary" />}
              title="语义证据图"
              hint="点击节点可将其作为因果链追踪起点"
            />
            <GraphControls
              nodeKind={nodeKind}
              edgeType={edgeType}
              minConfidence={minConfidence}
              rootNodeId={rootNodeId}
              maxDepth={maxDepth}
              onNodeKind={setNodeKind}
              onEdgeType={setEdgeType}
              onMinConfidence={setMinConfidence}
              onRootNodeId={setRootNodeId}
              onMaxDepth={setMaxDepth}
            />
            <div className="rounded-lg border border-border bg-card">
              <EvidenceGraph
                sessionId={sessionId}
                nodeKind={nodeKind}
                edgeType={edgeType}
                minConfidence={minConfidence}
                rootNodeId={rootNodeId}
                maxDepth={maxDepth}
                selectedNodeId={selectedNodeId}
                onSelectNode={(id) => setSelectedNodeId(id)}
              />
            </div>
          </section>

          {/* 因果链追踪 */}
          <section>
            <SectionHeader
              icon={<GitBranch className="h-4 w-4 text-primary" />}
              title="因果链追踪"
              hint={selectedNodeId ? `起点：${selectedNodeId}` : undefined}
            />
            <div className="rounded-lg border border-border bg-card">
              <TracePanel sessionId={sessionId} initialNodeId={selectedNodeId} />
            </div>
          </section>

          {/* 链路规则建议 */}
          <section>
            <SectionHeader
              icon={<BarChart3 className="h-4 w-4 text-primary" />}
              title="链路规则建议"
              hint="基于证据图自动归纳，可一键采纳复制"
            />
            <LinkRuleTable sessionId={sessionId} />
          </section>
        </div>
      )}
    </div>
  );
}
