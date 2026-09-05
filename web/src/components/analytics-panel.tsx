import { useMemo, useState } from "react";
import { useAggregateQuery } from "@/hooks/use-mcp";
import type { AggregateQueryResult, AggregatableField } from "@/types/analytics";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import { AlertTriangle, BarChart3, Search } from "lucide-react";

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

export function AnalyticsPanel({ sessionId }: { sessionId: string | null }) {
  const [expression, setExpression] = useState("name != \"\"");

  const agg = useAggregateQuery(expression, sessionId);

  const chips: AggregatableField[] = agg.data?.aggregatable_fields ?? [];

  function applyChip(f: AggregatableField) {
    const expr = f.alias ? f.alias : `${f.schema}.${f.field}`;
    setExpression(`${expr} > 0`);
  }

  return (
    <div className="h-full overflow-auto p-4 gt-scroll">
      {!sessionId ? (
        <EmptyState
          icon={<BarChart3 className="h-5 w-5" />}
          title="未选择会话"
          hint="在左侧选择一个会话后，这里会展示聚合统计指标。"
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
        </div>
      )}
    </div>
  );
}
