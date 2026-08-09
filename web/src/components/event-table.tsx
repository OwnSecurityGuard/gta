import { useState, useEffect, useMemo, Fragment, memo } from "react";
import { useDecodedData } from "@/hooks/use-mcp";
import { Table, TableHeader, TableBody, TableRow, TableHead, TableCell } from "@/components/ui/table";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import { ChevronLeft, ChevronRight, ChevronsLeft, Table2, SearchX, RotateCw } from "lucide-react";
import type { DecodedEvent } from "@/types/event";
import { unpackJsonStrings } from "@/lib/utils";

interface EventTableProps {
  sessionId: string | null;
  filter: string;
}

const PAGE_SIZE = 20;

/** 格式化 ISO 时间为本地时间 */
function formatTimestamp(isoStr: string): string {
  try {
    return new Date(isoStr).toLocaleString("zh-CN", {
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    });
  } catch {
    return isoStr;
  }
}

/** 截断 JSON 预览 */
function truncateJson(data: Record<string, unknown>, maxLen = 120): string {
  const raw = JSON.stringify(unpackJsonStrings(data));
  if (raw.length <= maxLen) return raw;
  return raw.slice(0, maxLen) + "…";
}

/** JSON 语法高亮组件 */
function HighlightedJson({ data }: { data: Record<string, unknown> }) {
  const formatted = useMemo(() => JSON.stringify(unpackJsonStrings(data), null, 2), [data]);

  return (
    <pre className="gta-json-pre">
      <HighlightedText text={formatted} />
    </pre>
  );
}

/** 递归高亮 JSON 文本 */
function HighlightedText({ text }: { text: string }) {
  const tokens = useMemo(() => tokenizeJson(text), [text]);

  return (
    <>
      {tokens.map((token, i) => {
        switch (token.type) {
          case "key":
            return <span key={i} className="gta-json-key">{token.text}</span>;
          case "string":
            return <span key={i} className="gta-json-string">{token.text}</span>;
          case "number":
            return <span key={i} className="gta-json-number">{token.text}</span>;
          case "boolean":
            return <span key={i} className="gta-json-boolean">{token.text}</span>;
          case "null":
            return <span key={i} className="gta-json-null">{token.text}</span>;
          default:
            return <span key={i} className="gta-json-punct">{token.text}</span>;
        }
      })}
    </>
  );
}

interface JsonToken {
  type: "key" | "string" | "number" | "boolean" | "null" | "punct";
  text: string;
}

/** 简易 JSON tokenizer — 按字符扫描生成带类型的 token */
function tokenizeJson(text: string): JsonToken[] {
  const tokens: JsonToken[] = [];
  let i = 0;
  let inString = false;
  let stringChar = "";

  while (i < text.length) {
    const ch = text[i]!;

    // 字符串
    if ((ch === '"' || ch === "'") && !inString) {
      inString = true;
      stringChar = ch;
      let end = i + 1;
      while (end < text.length && text[end] !== stringChar) {
        if (text[end] === "\\") end++; // 跳过转义
        end++;
      }
      const str = text.slice(i, end + 1);
      // 判断是否为 key（后面紧跟冒号）
      const afterStr = text.slice(end + 1).trimStart();
      if (afterStr.startsWith(":")) {
        tokens.push({ type: "key", text: str });
      } else {
        tokens.push({ type: "string", text: str });
      }
      i = end + 1;
      continue;
    }

    if (inString) {
      // 不应该到这里，但防御性处理
      tokens.push({ type: "string", text: ch });
      i++;
      continue;
    }

    // 数字（含负号）
    if (ch === "-" || (ch >= "0" && ch <= "9")) {
      let end = i + 1;
      while (end < text.length && /[\d.eE+\-]/.test(text[end]!)) end++;
      tokens.push({ type: "number", text: text.slice(i, end) });
      i = end;
      continue;
    }

    // true / false
    if (text.startsWith("true", i)) {
      tokens.push({ type: "boolean", text: "true" });
      i += 4;
      continue;
    }
    if (text.startsWith("false", i)) {
      tokens.push({ type: "boolean", text: "false" });
      i += 5;
      continue;
    }

    // null
    if (text.startsWith("null", i)) {
      tokens.push({ type: "null", text: "null" });
      i += 4;
      continue;
    }

    // 标点符号和空白
    if (ch === ":" || ch === "," || ch === "{" || ch === "}" || ch === "[" || ch === "]") {
      tokens.push({ type: "punct", text: ch });
    } else if (ch !== " " && ch !== "\n" && ch !== "\r" && ch !== "\t") {
      tokens.push({ type: "punct", text: ch });
    }

    i++;
  }

  return tokens;
}

/** 展开行：完整 JSON 查看 */
function ExpandedRow({ data }: { data: Record<string, unknown> }) {
  return (
    <TableRow className="gta-fade-in">
      <TableCell colSpan={3} className="bg-muted/30 p-4">
        <div className="gta-json-view">
          <HighlightedJson data={data} />
        </div>
      </TableCell>
    </TableRow>
  );
}

/** 单行事件：memo 化，展开某一行时其余行不重渲染，也不重算 JSON 预览 */
const EventRow = memo(function EventRow({
  event,
  isExpanded,
  onToggle,
}: {
  event: DecodedEvent;
  isExpanded: boolean;
  onToggle: (id: string) => void;
}) {
  return (
    <Fragment key={event.id}>
      <TableRow
        className="cursor-pointer"
        onClick={() => onToggle(event.id)}
        aria-expanded={isExpanded}
      >
        <TableCell className="font-mono text-xs whitespace-nowrap">
          {formatTimestamp(event.timestamp)}
        </TableCell>
        <TableCell className="text-xs tabular-nums">
          {event.raw_len}
        </TableCell>
        <TableCell className="font-mono text-xs">
          <pre className="whitespace-pre-wrap break-all">
            {truncateJson(event.data)}
          </pre>
        </TableCell>
      </TableRow>
      {isExpanded && <ExpandedRow data={event.data} />}
    </Fragment>
  );
});

export function EventTable({ sessionId, filter }: EventTableProps) {
  const [page, setPage] = useState<number>(0);
  const [expandedId, setExpandedId] = useState<string | null>(null);

  // 切换会话或筛选条件时回到第一页，避免停留在上一会话的页码导致偏移越界。
  useEffect(() => {
    setPage(0);
  }, [sessionId, filter]);

  const offset = page * PAGE_SIZE;

  const {
    data,
    isLoading,
    isError,
    error,
    refetch,
    isFetching,
    isPlaceholderData,
  } = useDecodedData(sessionId, {
    limit: PAGE_SIZE,
    offset,
    filter: filter || undefined,
  });

  const events = useMemo(() => data?.events ?? [], [data]);
  const totalMatched = data?.total_matched ?? 0;
  const totalPages = Math.ceil(totalMatched / PAGE_SIZE);

  function handleToggleExpand(eventId: string) {
    setExpandedId((prev) => (prev === eventId ? null : eventId));
  }

  if (!sessionId) {
    return (
      <EmptyState
        icon={<Table2 className="h-5 w-5" />}
        title="未选择会话"
        hint="在左侧会话列表中选择一个会话以查看解码出的协议事件。"
        className="h-64 justify-center"
      />
    );
  }

  // 首次加载（无历史数据）才显示骨架屏；keepPreviousData 接管翻页/筛选过渡
  if (isLoading) {
    return (
      <div className="space-y-2 p-4">
        {Array.from({ length: 5 }).map((_, i) => (
          <Skeleton key={i} className="h-12 w-full" />
        ))}
      </div>
    );
  }

  if (isError) {
    return (
      <div
        role="alert"
        className="flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive"
      >
        <span className="flex-1">加载失败：{error?.message ?? "未知错误"}</span>
        <Button variant="outline" size="sm" onClick={() => refetch()} className="h-7">
          <RotateCw className="h-3.5 w-3.5" />
          重试
        </Button>
      </div>
    );
  }

  if (events.length === 0) {
    return (
      <EmptyState
        icon={totalMatched === 0 ? <Table2 className="h-5 w-5" /> : <SearchX className="h-5 w-5" />}
        title={totalMatched === 0 ? "暂无解码数据" : "无匹配结果"}
        hint={
          totalMatched === 0
            ? "该会话尚未产生可解码的协议事件，或解码插件尚未绑定。"
            : "尝试调整筛选表达式，或清除筛选查看全部数据。"
        }
        className="h-64 justify-center"
      />
    );
  }

  return (
    <div className="space-y-3 relative">
      {/* 后台刷新指示：沿用上一页数据时显示顶部进度条，避免骨架屏闪烁 */}
      {isFetching && !isLoading && <div className="gta-loading-bar" aria-hidden="true" />}

      {/* 统计信息 */}
      <div
        className="flex items-center justify-between px-1 text-xs text-muted-foreground"
        aria-live="polite"
      >
        <span className="tabular-nums">
          共 {totalMatched} 条 · 当前第 {offset + 1}–{Math.min(offset + PAGE_SIZE, totalMatched)} 条
          {isPlaceholderData ? " · 更新中…" : ""}
        </span>
      </div>

      {/* 数据表格 */}
      <Table className="gta-table">
        <TableHeader>
          <TableRow>
            <TableHead className="w-48">Timestamp</TableHead>
            <TableHead className="w-20">Raw Len</TableHead>
            <TableHead>Data</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {events.map((event: DecodedEvent) => (
            <EventRow
              key={event.id}
              event={event}
              isExpanded={expandedId === event.id}
              onToggle={handleToggleExpand}
            />
          ))}
        </TableBody>
      </Table>

      {/* 分页控件 */}
      {totalPages > 1 && (
        <div className="flex items-center justify-center gap-2 pt-2">
          <Button
            variant="outline"
            size="icon"
            className="h-8 w-8"
            onClick={() => setPage(0)}
            disabled={page === 0}
            aria-label="第一页"
          >
            <ChevronsLeft className="h-4 w-4" />
          </Button>
          <Button
            variant="outline"
            size="icon"
            className="h-8 w-8"
            onClick={() => setPage((p) => Math.max(0, p - 1))}
            disabled={page === 0}
            aria-label="上一页"
          >
            <ChevronLeft className="h-4 w-4" />
          </Button>
          <span className="px-2 text-sm tabular-nums">
            {page + 1} / {totalPages}
          </span>
          <Button
            variant="outline"
            size="icon"
            className="h-8 w-8"
            onClick={() => setPage((p) => Math.min(totalPages - 1, p + 1))}
            disabled={page >= totalPages - 1}
            aria-label="下一页"
          >
            <ChevronRight className="h-4 w-4" />
          </Button>
        </div>
      )}
    </div>
  );
}
