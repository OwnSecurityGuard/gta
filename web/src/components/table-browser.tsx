import { useEffect, useState } from "react";
import { useQueryCaptureTable } from "@/hooks/use-mcp";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import { AlertTriangle, Table2, ChevronLeft, ChevronRight } from "lucide-react";

/** query_capture_table 的允许表（与后端 allowlist 保持一致）。 */
const TABLES = [
  "events",
  "raw_packets",
  "state_changes",
  "aggregated_metrics",
  "event_index",
  "plugin_debug_access",
];

/** 将任意值渲染成可显示的字符串。 */
function cellText(v: unknown): string {
  if (v === null || v === undefined) return "";
  if (typeof v === "object") {
    try {
      return JSON.stringify(v);
    } catch {
      return String(v);
    }
  }
  return String(v);
}

export function TableBrowser({ sessionId }: { sessionId: string | null }) {
  const [table, setTable] = useState("events");
  const [limit, setLimit] = useState(100);
  const [offset, setOffset] = useState(0);

  // 切换会话或表时回到第一页。
  useEffect(() => {
    setOffset(0);
  }, [sessionId, table]);

  const q = useQueryCaptureTable(sessionId, table, { limit, offset });

  // 列：取所有行键的并集（保证稀疏行也能展示）。
  const cols = new Set<string>();
  const rows = q.data?.rows ?? [];
  for (const r of rows) {
    for (const k of Object.keys(r)) cols.add(k);
  }
  const colList = Array.from(cols);

  if (!sessionId) {
    return (
      <EmptyState
        icon={<Table2 className="h-5 w-5" />}
        title="未选择会话"
        hint="在左侧选择一个会话后，这里可以只读浏览内部投影/审计表。"
      />
    );
  }

  return (
    <div className="flex h-full flex-col overflow-hidden">
      <div className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-3">
        <Table2 className="h-4 w-4 text-primary" />
        <select
          className="h-8 rounded-md border border-input bg-background px-2 text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
          value={table}
          onChange={(e) => setTable(e.target.value)}
        >
          {TABLES.map((t) => (
            <option key={t} value={t}>
              {t}
            </option>
          ))}
        </select>
        <label className="flex items-center gap-1 text-xs text-muted-foreground">
          每页
          <select
            className="h-8 rounded-md border border-input bg-background px-2 text-xs"
            value={limit}
            onChange={(e) => {
              setLimit(Number(e.target.value));
              setOffset(0);
            }}
          >
            <option value={50}>50</option>
            <option value={100}>100</option>
            <option value={500}>500</option>
          </select>
        </label>
        <span className="text-xs text-muted-foreground">
          共 {q.data?.count ?? 0} 行 · 本页 {rows.length}
        </span>
        <div className="ml-auto flex items-center gap-1">
          <button
            className="inline-flex h-8 items-center gap-1 rounded-md border border-input px-2 text-xs disabled:opacity-40"
            disabled={offset <= 0 || q.isFetching}
            onClick={() => setOffset((o) => Math.max(0, o - limit))}
          >
            <ChevronLeft className="h-3.5 w-3.5" />
            上一页
          </button>
          <button
            className="inline-flex h-8 items-center gap-1 rounded-md border border-input px-2 text-xs disabled:opacity-40"
            disabled={rows.length < limit || q.isFetching}
            onClick={() => setOffset((o) => o + limit)}
          >
            下一页
            <ChevronRight className="h-3.5 w-3.5" />
          </button>
        </div>
      </div>

      <div className="min-h-0 flex-1 overflow-auto gta-scroll">
        {q.isLoading ? (
          <div className="space-y-2 p-4">
            {Array.from({ length: 8 }).map((_, i) => (
              <Skeleton key={i} className="h-7 w-full rounded" />
            ))}
          </div>
        ) : q.isError ? (
          <div className="m-4 flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive">
            <AlertTriangle className="h-4 w-4 shrink-0" />
            <span>查询失败：{q.error?.message}</span>
          </div>
        ) : rows.length === 0 ? (
          <EmptyState
            icon={<Table2 className="h-5 w-5" />}
            title="该表无数据"
            hint={`会话的 ${table} 表为空。`}
          />
        ) : (
          <table className="w-full border-collapse text-xs">
            <thead className="sticky top-0 bg-muted/80 backdrop-blur">
              <tr className="text-muted-foreground">
                {colList.map((c) => (
                  <th key={c} className="border-b border-border px-2 py-1.5 text-left font-medium">
                    {c}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {rows.map((r, i) => (
                <tr key={i} className="border-b border-border/60 hover:bg-muted/40">
                  {colList.map((c) => (
                    <td
                      key={c}
                      className="max-w-[280px] truncate px-2 py-1.5 font-mono text-foreground/90"
                      title={cellText(r[c])}
                    >
                      {cellText(r[c])}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
