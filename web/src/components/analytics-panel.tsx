import { useMemo, useState } from "react";
import { useAggregateQuery, useAnalyzePatterns } from "@/hooks/use-mcp";
import type {
  AggregateQueryResult,
  AnalyzePatternsResult,
  AggregatableField,
  PatternEventType,
} from "@/types/analytics";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import { AlertTriangle, BarChart3, Search, Workflow } from "lucide-react";

function fmtNumber(n: number): string {
  if (!Number.isFinite(n)) return String(n);
  return Math.round(n).toLocaleString();
}

/** 将聚合指标投影为 KPI 卡片：相同 name 的多个 group 合并为该指标的多条记录。 */
function aggregateMetricsToCards(metrics: AggregateQueryResult["metrics"]) {
  // 按 name 分组，每组取第一个 group 作为标签
  const byName = new Map<string, { value: number; groupLabel: string }[]>();
  for (const m of metrics) {
    const arr = byName.get(m.name) ?? [];
    const groupLabel = Object.entries(m.group ?? {})
      .map(([k, v]) => `${k}=${v}`)
      .join(" ");
    arr.push({ value: m.value, groupLabel });
    byName.set(m.name, arr);
  }
  const cards: { name: string; value: number; groupLabel: string }[] = [];
  for (const [name, arr] of byName) {
    for (const item of arr) cards.push({ name, ...item });
  }
  return cards;
}

function MetricCards({ data }: { data: AggregateQueryResult }) {
  const cards = useMemo(() => aggregateMetricsToCards(data.metrics ?? []), [data]);
  if (cards.length === 0) {
    return (
      <div className="rounded-lg border border-dashed border-border p-6 text-center text-sm text-muted-foreground">
        无匹配指标（表达式未命中任何聚合记录）。
      </div>
    );
  }
  return (
    <div className="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-4">
      {cards.map((c, i) => (
        <div key={`${c.name}-${i}`} className="rounded-lg bg-secondary p-3">
          <div className="text-xs text-muted-foreground truncate" title={c.name}>
            {c.name}
          </div>
          <div className="mt-1 text-2xl font-medium tabular-nums">{fmtNumber(c.value)}</div>
          {c.groupLabel && (
            <div className="mt-0.5 truncate font-mono text-[11px] text-muted-foreground/80" title={c.groupLabel}>
              {c.groupLabel}
            </div>
          )}
        </div>
      ))}
    </div>
  );
}

function MessageTypeBars({ rows }: { rows: PatternEventType[] }) {
  const max = rows.reduce((m, r) => Math.max(m, r.count), 0);
  return (
    <div className="space-y-1.5">
      {rows.map((r) => (
        <div key={r.event_type} className="flex items-center gap-2 text-xs">
          <span className="w-44 shrink-0 truncate font-mono" title={r.event_type}>
            {r.event_type}
          </span>
          <div className="h-2 flex-1 overflow-hidden rounded-full bg-muted">
            <div
              className="h-full rounded-full"
              style={{
                width: `${max ? (r.count / max) * 100 : 0}%`,
                backgroundImage: "linear-gradient(90deg, var(--color-primary), var(--color-info))",
              }}
            />
          </div>
          <span className="w-12 shrink-0 text-right tabular-nums">{fmtNumber(r.count)}</span>
        </div>
      ))}
    </div>
  );
}

function PatternCard({
  title,
  children,
  empty,
}: {
  title: string;
  children: React.ReactNode;
  empty?: boolean;
}) {
  return (
    <div className="gta-card p-4">
      <h3 className="mb-3 text-sm font-semibold">{title}</h3>
      {empty ? (
        <p className="text-xs text-muted-foreground">无数据</p>
      ) : (
        children
      )}
    </div>
  );
}

function PatternsGrid({ data }: { data: AnalyzePatternsResult }) {
  return (
    <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
      <PatternCard title="消息类型分布" empty={!data.event_types || data.event_types.length === 0}>
        <MessageTypeBars rows={data.event_types ?? []} />
      </PatternCard>

      <PatternCard title="流统计" empty={!data.flows || data.flows.length === 0}>
        <div className="overflow-x-auto">
          <table className="w-full text-xs">
            <thead>
              <tr className="text-muted-foreground">
                <th className="py-1 pr-2 text-left font-medium">flow</th>
                <th className="py-1 pr-2 text-right font-medium">事件</th>
                <th className="py-1 pr-2 text-right font-medium">c2s</th>
                <th className="py-1 text-right font-medium">s2c</th>
              </tr>
            </thead>
            <tbody>
              {(data.flows ?? []).map((f) => (
                <tr key={f.flow_id} className="border-t border-border">
                  <td className="py-1 pr-2 font-mono truncate max-w-[120px]">{f.flow_id}</td>
                  <td className="py-1 pr-2 text-right tabular-nums">{fmtNumber(f.event_count)}</td>
                  <td className="py-1 pr-2 text-right tabular-nums">{fmtNumber(f.c2s)}</td>
                  <td className="py-1 text-right tabular-nums">{fmtNumber(f.s2c)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </PatternCard>

      <PatternCard title="状态变更模式" empty={!data.state_change_patterns || data.state_change_patterns.length === 0}>
        <div className="max-h-44 space-y-1 overflow-auto gta-scroll">
          {(data.state_change_patterns ?? []).map((p, i) => (
            <div key={i} className="flex items-center justify-between gap-2 text-xs">
              <span className="truncate font-mono" title={`${p.subject_type} ${p.op} ${p.path}`}>
                {p.subject_type} <span className="text-muted-foreground">· {p.op}</span>
              </span>
              <span className="shrink-0 tabular-nums text-muted-foreground">{fmtNumber(p.count)}</span>
            </div>
          ))}
        </div>
      </PatternCard>

      <PatternCard title="相关性流" empty={!data.correlated_flows || data.correlated_flows.length === 0}>
        <div className="space-y-1">
          {(data.correlated_flows ?? []).map((c) => (
            <div key={c.flow_id} className="flex items-center justify-between gap-2 text-xs">
              <span className="truncate font-mono" title={c.flow_id}>{c.flow_id}</span>
              <span className="shrink-0 tabular-nums text-muted-foreground">{fmtNumber(c.correlation_count)}</span>
            </div>
          ))}
        </div>
      </PatternCard>

      <PatternCard title="证据图结构" empty={!data.evidence_graph_nodes && !data.evidence_graph_edges}>
        <div className="space-y-2 text-xs">
          <div>
            <div className="mb-1 text-muted-foreground">节点（按 kind）</div>
            {(data.evidence_graph_nodes ?? []).map((n) => (
              <div key={n.kind} className="flex items-center justify-between gap-2">
                <span className="truncate">{n.kind}</span>
                <span className="tabular-nums text-muted-foreground">{fmtNumber(n.node_count)}</span>
              </div>
            ))}
            {(!data.evidence_graph_nodes || data.evidence_graph_nodes.length === 0) && (
              <span className="text-muted-foreground">—</span>
            )}
          </div>
          <div>
            <div className="mb-1 text-muted-foreground">边（按 type）</div>
            {(data.evidence_graph_edges ?? []).map((e) => (
              <div key={e.type} className="flex items-center justify-between gap-2">
                <span className="truncate">{e.type}</span>
                <span className="tabular-nums text-muted-foreground">
                  {fmtNumber(e.edge_count)} · 置信 {e.avg_confidence.toFixed(2)}
                </span>
              </div>
            ))}
            {(!data.evidence_graph_edges || data.evidence_graph_edges.length === 0) && (
              <span className="text-muted-foreground">—</span>
            )}
          </div>
        </div>
      </PatternCard>

      <PatternCard title="方向分布" empty={!data.direction_distribution || data.direction_distribution.length === 0}>
        <div className="space-y-1">
          {(data.direction_distribution ?? []).map((d) => (
            <div key={d.direction} className="flex items-center justify-between gap-2 text-xs">
              <span className="truncate">{d.direction}</span>
              <span className="tabular-nums text-muted-foreground">{fmtNumber(d.count)}</span>
            </div>
          ))}
        </div>
      </PatternCard>
    </div>
  );
}

export function AnalyticsPanel({ sessionId }: { sessionId: string | null }) {
  const [expression, setExpression] = useState("name != \"\"");

  const agg = useAggregateQuery(expression, sessionId);
  const patterns = useAnalyzePatterns(sessionId);

  const chips: AggregatableField[] = agg.data?.aggregatable_fields ?? [];

  function applyChip(f: AggregatableField) {
    const expr = f.alias ? f.alias : `${f.schema}.${f.field}`;
    setExpression(`${expr} > 0`);
  }

  return (
    <div className="h-full overflow-auto p-4 gta-scroll">
      {!sessionId ? (
        <EmptyState
          icon={<BarChart3 className="h-5 w-5" />}
          title="未选择会话"
          hint="在左侧选择一个会话后，这里会展示聚合统计与协议模式分析。"
        />
      ) : (
        <div className="space-y-5">
          {/* 聚合查询框 */}
          <section>
            <div className="mb-2 flex items-center gap-2 text-sm font-semibold">
              <BarChart3 className="h-4 w-4 text-primary" />
              聚合指标
            </div>
            <div className="relative">
              <Search className="pointer-events-none absolute left-2.5 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
              <input
                value={expression}
                onChange={(e) => setExpression(e.target.value)}
                aria-label="聚合表达式"
                spellCheck={false}
                placeholder='如 name == "http_req_count" && value > 0'
                className="h-9 w-full rounded-md border border-input bg-background pl-8 pr-3 font-mono text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
              />
            </div>
            {chips.length > 0 && (
              <div className="mt-2 flex flex-wrap items-center gap-1.5">
                <span className="text-xs text-muted-foreground">可聚合字段</span>
                {chips.map((f) => (
                  <button
                    key={`${f.schema}.${f.field}`}
                    type="button"
                    onClick={() => applyChip(f)}
                    className="rounded-full bg-primary-muted px-2 py-0.5 text-xs text-accent-foreground transition-colors hover:bg-primary/15"
                    title={`${f.schema}.${f.field}`}
                  >
                    {f.alias ?? `${f.schema}.${f.field}`}
                  </button>
                ))}
              </div>
            )}

            <div className="mt-3">
              {agg.isLoading ? (
                <div className="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-4">
                  {Array.from({ length: 4 }).map((_, i) => (
                    <Skeleton key={i} className="h-20 w-full rounded-lg" />
                  ))}
                </div>
              ) : agg.isError ? (
                <div
                  role="alert"
                  className="flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive"
                >
                  <AlertTriangle className="h-4 w-4 shrink-0" />
                  <span>聚合查询失败：{agg.error?.message}</span>
                </div>
              ) : agg.data ? (
                <MetricCards data={agg.data} />
              ) : null}
            </div>
          </section>

          {/* 协议模式分析 */}
          <section>
            <div className="mb-2 flex items-center gap-2 text-sm font-semibold">
              <Workflow className="h-4 w-4 text-primary" />
              协议模式分析
            </div>
            {patterns.isLoading ? (
              <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
                {Array.from({ length: 6 }).map((_, i) => (
                  <Skeleton key={i} className="h-40 w-full rounded-lg" />
                ))}
              </div>
            ) : patterns.isError ? (
              <div
                role="alert"
                className="flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive"
              >
                <AlertTriangle className="h-4 w-4 shrink-0" />
                <span>模式分析失败：{patterns.error?.message}</span>
              </div>
            ) : patterns.data ? (
              <PatternsGrid data={patterns.data} />
            ) : null}
          </section>
        </div>
      )}
    </div>
  );
}
