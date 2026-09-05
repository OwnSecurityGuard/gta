import { useCaptureSchema } from "@/hooks/use-mcp";
import type { CaptureSchemaResult, SchemaSource, SchemaRule } from "@/types/event";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import { AlertTriangle, Database, KeyRound, ListTree } from "lucide-react";

function SourceCard({ source }: { source: SchemaSource }) {
  return (
    <div className="gt-card p-3">
      <div className="mb-2 flex items-center gap-2">
        <Database className="h-4 w-4 text-primary" />
        <span className="text-sm font-medium">{source.name}</span>
      </div>
      <p className="mb-2 text-xs text-muted-foreground">{source.description}</p>
      <div className="overflow-x-auto gt-scroll rounded-md border border-border">
        <table className="w-full text-xs">
          <thead>
            <tr className="bg-muted/60 text-muted-foreground">
              <th className="py-1 px-2 text-left font-medium">列</th>
              <th className="py-1 px-2 text-left font-medium">类型</th>
              <th className="py-1 px-2 text-left font-medium">说明</th>
            </tr>
          </thead>
          <tbody>
            {source.columns.map((c) => (
              <tr key={c.name} className="border-t border-border">
                <td className="py-1 px-2 font-mono">{c.name}</td>
                <td className="py-1 px-2 font-mono text-info">{c.type}</td>
                <td className="py-1 px-2 text-muted-foreground">{c.description}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function RuleCard({ rule }: { rule: SchemaRule }) {
  return (
    <div className="gt-card p-3">
      <div className="mb-1 flex items-center gap-2">
        <ListTree className="h-4 w-4 text-primary" />
        <span className="text-sm font-medium">{rule.name}</span>
        <span className="rounded bg-primary-muted px-1.5 py-0.5 font-mono text-[11px] text-accent-foreground">
          {rule.type}
        </span>
      </div>
      <dl className="space-y-0.5 text-xs text-muted-foreground">
        <div className="flex gap-2">
          <dt className="w-16 shrink-0">filter</dt>
          <dd className="font-mono">{rule.filter}</dd>
        </div>
        <div className="flex gap-2">
          <dt className="w-16 shrink-0">window</dt>
          <dd className="font-mono">{rule.window}</dd>
        </div>
        <div className="flex gap-2">
          <dt className="w-16 shrink-0">group_by</dt>
          <dd className="font-mono">{rule.group_by.join(", ") || "—"}</dd>
        </div>
        <div className="flex gap-2">
          <dt className="w-16 shrink-0">value</dt>
          <dd className="font-mono">{rule.value}</dd>
        </div>
        <div className="flex gap-2">
          <dt className="w-16 shrink-0">output</dt>
          <dd className="font-mono">{rule.output}</dd>
        </div>
      </dl>
    </div>
  );
}

export function SchemaExplorer({ sessionId }: { sessionId: string | null }) {
  const { data, isLoading, isError, error } = useCaptureSchema(sessionId);

  if (!sessionId) {
    return (
      <EmptyState
        icon={<Database className="h-5 w-5" />}
        title="未选择会话"
        hint="在左侧选择一个会话后，这里会展示该会话的捕获 schema（数据源/可查询字段/聚合规则）。"
      />
    );
  }

  if (isLoading) {
    return (
      <div className="space-y-3 p-4">
        {Array.from({ length: 3 }).map((_, i) => (
          <Skeleton key={i} className="h-28 w-full rounded-lg" />
        ))}
      </div>
    );
  }

  if (isError) {
    return (
      <div className="m-4 flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive">
        <AlertTriangle className="h-4 w-4 shrink-0" />
        <span>schema 加载失败：{error?.message}</span>
      </div>
    );
  }

  const schema = data as CaptureSchemaResult | undefined;
  const sources = schema?.sources ?? [];
  const queryFields = schema?.query_fields ?? [];
  const rules = schema?.rules ?? [];
  const examples = schema?.examples;

  if (!schema || (sources.length === 0 && rules.length === 0 && queryFields.length === 0)) {
    return (
      <EmptyState
        icon={<Database className="h-5 w-5" />}
        title="无 schema 信息"
        hint="该会话尚未生成 schema（可能还没有解码数据或插件未声明 schema）。"
      />
    );
  }

  return (
    <div className="h-full overflow-auto p-4 gt-scroll space-y-5">
      <section>
        <div className="mb-2 flex items-center gap-2 text-sm font-semibold">
          <Database className="h-4 w-4 text-primary" />
          数据源（{sources.length}）
        </div>
        <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
          {sources.map((s) => (
            <SourceCard key={s.name} source={s} />
          ))}
        </div>
      </section>

      {queryFields.length > 0 && (
        <section>
          <div className="mb-2 flex items-center gap-2 text-sm font-semibold">
            <KeyRound className="h-4 w-4 text-primary" />
            可查询字段（list_decoded_data filter 可用）
          </div>
          <div className="flex flex-wrap gap-1.5">
            {queryFields.map((f) => (
              <span
                key={f.name}
                className="rounded-md border border-border bg-muted px-2 py-0.5 font-mono text-[11px] text-muted-foreground"
                title={f.description}
              >
                {f.name}
                <span className="ml-1 text-info">{f.type}</span>
              </span>
            ))}
          </div>
        </section>
      )}

      {rules.length > 0 && (
        <section>
          <div className="mb-2 flex items-center gap-2 text-sm font-semibold">
            <ListTree className="h-4 w-4 text-primary" />
            聚合规则（{rules.length}）
          </div>
          <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
            {rules.map((r) => (
              <RuleCard key={r.name} rule={r} />
            ))}
          </div>
        </section>
      )}

      {examples && (
        <section>
          <div className="mb-2 text-sm font-semibold">查询示例</div>
          <div className="space-y-2">
            {examples.aggregate_query && examples.aggregate_query.length > 0 && (
              <div>
                <p className="mb-1 text-xs text-muted-foreground">aggregate_query</p>
                <ul className="space-y-1">
                  {examples.aggregate_query.map((ex, i) => (
                    <li key={i} className="rounded bg-muted px-2 py-1 font-mono text-[11px] text-foreground">
                      {ex}
                    </li>
                  ))}
                </ul>
              </div>
            )}
            {examples.list_decoded_data_filter && examples.list_decoded_data_filter.length > 0 && (
              <div>
                <p className="mb-1 text-xs text-muted-foreground">list_decoded_data filter</p>
                <ul className="space-y-1">
                  {examples.list_decoded_data_filter.map((ex, i) => (
                    <li key={i} className="rounded bg-muted px-2 py-1 font-mono text-[11px] text-foreground">
                      {ex}
                    </li>
                  ))}
                </ul>
              </div>
            )}
          </div>
        </section>
      )}
    </div>
  );
}
