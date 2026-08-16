import { useSuggestLinkRules } from "@/hooks/use-mcp";
import type { LinkRuleSuggestion } from "@/types/evidence";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import { Button } from "@/components/ui/button";
import { toast } from "@/components/ui/toast";
import { AlertTriangle, Lightbulb, Copy, Check } from "lucide-react";
import { useState } from "react";

function Row({ rule, onCopied }: { rule: LinkRuleSuggestion; onCopied: (tpl: string) => void }) {
  const [copied, setCopied] = useState(false);

  async function handleCopy() {
    const text = rule.rule_template;
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      // 退化方案：clipboard API 不可用时，用临时 textarea 复制
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      try {
        document.execCommand("copy");
      } catch {
        /* noop */
      }
      document.body.removeChild(ta);
    }
    setCopied(true);
    onCopied(text);
    window.setTimeout(() => setCopied(false), 1500);
  }

  return (
    <tr className="border-t border-border align-top">
      <td className="py-2 pr-3">
        <span className="rounded bg-primary-muted px-1.5 py-0.5 font-mono text-[11px] text-accent-foreground">
          {rule.edge_type}
        </span>
      </td>
      <td className="py-2 pr-3 font-mono text-[11px] text-muted-foreground">
        {rule.source_type} → {rule.target_type}
      </td>
      <td className="py-2 pr-3 text-right tabular-nums">{rule.occurrences}</td>
      <td className="py-2 pr-3 text-right tabular-nums">{rule.avg_confidence.toFixed(2)}</td>
      <td className="py-2 pr-3">
        <code className="block max-w-[320px] whitespace-pre-wrap break-words rounded bg-muted px-2 py-1 font-mono text-[11px] text-foreground">
          {rule.rule_template}
        </code>
      </td>
      <td className="py-2 pr-1 text-right">
        <Button
          variant="outline"
          size="sm"
          className="h-7"
          onClick={handleCopy}
          title="复制规则模板"
        >
          {copied ? <Check className="h-3.5 w-3.5 text-success" /> : <Copy className="h-3.5 w-3.5" />}
          {copied ? "已复制" : "采纳"}
        </Button>
      </td>
    </tr>
  );
}

export function LinkRuleTable({ sessionId }: { sessionId: string | null }) {
  const rules = useSuggestLinkRules(sessionId);

  if (!sessionId) {
    return (
      <EmptyState
        icon={<Lightbulb className="h-5 w-5" />}
        title="未选择会话"
        hint="在左侧选择一个会话后，这里会展示自动生成的链路规则建议。"
      />
    );
  }

  if (rules.isLoading) {
    return (
      <div className="space-y-2">
        {Array.from({ length: 3 }).map((_, i) => (
          <Skeleton key={i} className="h-10 w-full rounded-md" />
        ))}
      </div>
    );
  }

  if (rules.isError) {
    return (
      <div
        role="alert"
        className="flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive"
      >
        <AlertTriangle className="h-4 w-4 shrink-0" />
        <span>规则建议生成失败：{rules.error?.message}</span>
      </div>
    );
  }

  const data = rules.data;
  const items = data?.suggestions ?? [];

  if (!data || items.length === 0) {
    return (
      <EmptyState
        icon={<Lightbulb className="h-5 w-5" />}
        title="暂无规则建议"
        hint="证据图边数或置信度不足，未生成可用链路规则。试着先在证据图中构建更多实体/状态变更关系。"
      />
    );
  }

  function handleCopied(tpl: string) {
    toast.success("已复制规则模板", tpl.length > 60 ? tpl.slice(0, 60) + "…" : tpl);
  }

  return (
    <div className="space-y-2">
      <div className="text-xs text-muted-foreground">
        共 {items.length} 条建议 · 证据图 {data?.total_nodes ?? 0} 节点 / {data?.total_edges ?? 0} 边
      </div>
      <div className="overflow-x-auto gta-scroll rounded-lg border border-border">
        <table className="w-full text-xs">
          <thead>
            <tr className="bg-muted/60 text-muted-foreground">
              <th className="py-2 pr-3 text-left font-medium">edge_type</th>
              <th className="py-2 pr-3 text-left font-medium">source → target</th>
              <th className="py-2 pr-3 text-right font-medium">次数</th>
              <th className="py-2 pr-3 text-right font-medium">平均置信</th>
              <th className="py-2 pr-3 text-left font-medium">规则模板</th>
              <th className="py-2 pr-1 text-right font-medium">操作</th>
            </tr>
          </thead>
          <tbody>
            {items.map((r, i) => (
              <Row key={`${r.edge_type}-${r.source_type}-${r.target_type}-${i}`} rule={r} onCopied={handleCopied} />
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
