import { useState, useMemo, useEffect, Fragment, memo } from "react";
import { useRawPackets, useListPlugins, useDecodeRawPackets } from "@/hooks/use-mcp";
import { Table, TableHeader, TableBody, TableRow, TableHead, TableCell } from "@/components/ui/table";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import { toast } from "@/components/ui/toast";
import { ChevronLeft, ChevronRight, ChevronsLeft, Network, RotateCw } from "lucide-react";
import type { RawPacket } from "@/types/raw-packet";

interface RawPacketTableProps {
  sessionId: string | null;
  /** 解码成功后由父组件触发切换到协议数据 Tab */
  onDecoded?: () => void;
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

/** base64 解码为 Uint8Array */
function base64ToBytes(b64: string): Uint8Array {
  const binary = atob(b64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
}

/** 将字节数组格式化为 hex dump（偏移 | hex | ascii） */
function hexDump(bytes: Uint8Array, maxBytes = 4096): string {
  const truncated = bytes.length > maxBytes;
  const slice = truncated ? bytes.slice(0, maxBytes) : bytes;
  const lines: string[] = [];

  for (let offset = 0; offset < slice.length; offset += 16) {
    const chunk = slice.slice(offset, offset + 16);
    const hexParts: string[] = [];
    const asciiParts: string[] = [];

    for (let i = 0; i < 16; i++) {
      if (i < chunk.length) {
        hexParts.push(chunk[offset + i]!.toString(16).padStart(2, "0"));
        const ch = chunk[offset + i]!;
        asciiParts.push(ch >= 0x20 && ch < 0x7f ? String.fromCharCode(ch) : ".");
      } else {
        hexParts.push("  ");
        asciiParts.push(" ");
      }
    }

    const offsetStr = offset.toString(16).padStart(8, "0");
    const hexStr = hexParts.slice(0, 8).join(" ") + "  " + hexParts.slice(8).join(" ");
    const asciiStr = asciiParts.join("");
    lines.push(`${offsetStr}  ${hexStr}  |${asciiStr}|`);
  }

  if (truncated) {
    lines.push(`... (${bytes.length} bytes total, showing first ${maxBytes})`);
  }

  return lines.join("\n");
}

/** payload 预览（单行截断） */
function payloadPreview(b64: string, maxLen = 48): string {
  try {
    const bytes = base64ToBytes(b64);
    const hex: string[] = [];
    for (let i = 0; i < Math.min(bytes.length, maxLen); i++) {
      hex.push(bytes[i]!.toString(16).padStart(2, "0"));
    }
    const suffix = bytes.length > maxLen ? "..." : "";
    return hex.join(" ") + suffix;
  } catch {
    return "(decode error)";
  }
}

/** 展开行：完整 hex dump */
function ExpandedHexRow({ payload }: { payload: string }) {
  const hex = useMemo(() => {
    try {
      return hexDump(base64ToBytes(payload));
    } catch {
      return "(decode error)";
    }
  }, [payload]);

  return (
    <TableRow className="gta-fade-in">
      <TableCell colSpan={6} className="bg-muted/30 p-4">
        <pre className="text-xs font-mono whitespace-pre overflow-x-auto">
          {hex}
        </pre>
      </TableCell>
    </TableRow>
  );
}

/** 单行原始包：memo 化，展开某一行时其余行不重渲染 */
const RawPacketRow = memo(function RawPacketRow({
  pkt,
  isExpanded,
  onToggle,
}: {
  pkt: RawPacket;
  isExpanded: boolean;
  onToggle: (id: string) => void;
}) {
  return (
    <Fragment key={pkt.id}>
      <TableRow
        className="cursor-pointer"
        onClick={() => onToggle(pkt.id)}
        aria-expanded={isExpanded}
      >
        <TableCell className="font-mono text-xs whitespace-nowrap">
          {formatTimestamp(pkt.timestamp)}
        </TableCell>
        <TableCell className="max-w-[160px] truncate font-mono text-xs" title={pkt.src}>
          {pkt.src}
        </TableCell>
        <TableCell className="max-w-[160px] truncate font-mono text-xs" title={pkt.dst}>
          {pkt.dst}
        </TableCell>
        <TableCell className="font-mono text-xs">
          {pkt.protocol}
        </TableCell>
        <TableCell className="text-xs tabular-nums">
          {pkt.payload_len}
        </TableCell>
        <TableCell className="font-mono text-xs">
          <span className="text-muted-foreground">
            {payloadPreview(pkt.payload)}
          </span>
        </TableCell>
      </TableRow>
      {isExpanded && <ExpandedHexRow payload={pkt.payload} />}
    </Fragment>
  );
});

export function RawPacketTable({ sessionId, onDecoded }: RawPacketTableProps) {
  const [page, setPage] = useState<number>(0);
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [selectedPlugin, setSelectedPlugin] = useState<string>("");
  const [clearExisting, setClearExisting] = useState<boolean>(true);

  const { data: pluginsData } = useListPlugins();
  const decodeMutation = useDecodeRawPackets();
  const plugins = pluginsData?.plugins ?? [];

  // 插件列表加载后默认选第一个
  useEffect(() => {
    if (!selectedPlugin && plugins.length > 0) {
      setSelectedPlugin(plugins[0]!.name);
    }
  }, [plugins, selectedPlugin]);

  function handleDecode() {
    if (!sessionId || !selectedPlugin) return;
    decodeMutation.mutate(
      { sessionId, plugin: selectedPlugin, clearExisting },
      {
        onSuccess: (res) => {
          toast.success(
            "解码完成",
            `成功 ${res?.decoded ?? 0} / 失败 ${res?.decode_errors ?? 0}（共 ${res?.total_raw ?? 0} 包）`,
          );
        },
        onError: (err) => {
          toast.error("解码失败", err.message);
        },
      },
    );
  }

  const offset = page * PAGE_SIZE;

  const {
    data,
    isLoading,
    isError,
    error,
    refetch,
    isFetching,
    isPlaceholderData,
  } = useRawPackets(sessionId, {
    limit: PAGE_SIZE,
    offset,
  });

  const packets = useMemo(() => data?.packets ?? [], [data]);
  const count = data?.count ?? 0;
  // list_raw_packets 返回当前页的 count，不是 total。用 count 估算分页。
  const totalPages = Math.ceil(count / PAGE_SIZE);

  function handleToggleExpand(packetId: string) {
    setExpandedId((prev) => (prev === packetId ? null : packetId));
  }

  if (!sessionId) {
    return (
      <EmptyState
        icon={<Network className="h-5 w-5" />}
        title="未选择会话"
        hint="在左侧会话列表中选择一个会话以查看抓包得到的原始包。"
        className="h-64 justify-center"
      />
    );
  }

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

  if (packets.length === 0) {
    return (
      <EmptyState
        icon={<Network className="h-5 w-5" />}
        title="暂无原始包"
        hint="该会话尚未抓取到任何原始包数据。"
        className="h-64 justify-center"
      />
    );
  }

  return (
    <div className="space-y-3 relative">
      {/* 后台刷新指示 */}
      {isFetching && !isLoading && <div className="gta-loading-bar" aria-hidden="true" />}

      {/* 解码工具栏：用插件对离线会话原始包批量解码，结果写入 decoded_events */}
      {sessionId && (
        <div className="flex flex-wrap items-center gap-3 rounded-lg border border-border bg-card p-2.5 shadow-sm">
          <span className="shrink-0 text-xs font-medium text-muted-foreground">解码插件</span>
          <select
            className="h-8 rounded-md border border-input bg-background px-2 text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
            value={selectedPlugin}
            onChange={(e) => setSelectedPlugin(e.target.value)}
            disabled={plugins.length === 0}
          >
            {plugins.length === 0 ? (
              <option value="">无可用插件</option>
            ) : (
              plugins.map((p) => (
                <option key={p.name} value={p.name}>{p.name}</option>
              ))
            )}
          </select>
          <label className="flex cursor-pointer select-none items-center gap-1 text-xs text-muted-foreground">
            <input
              type="checkbox"
              checked={clearExisting}
              onChange={(e) => setClearExisting(e.target.checked)}
              className="h-3.5 w-3.5 rounded border-input accent-primary"
            />
            清空已有解码结果
          </label>
          <Button
            size="sm"
            onClick={handleDecode}
            disabled={!selectedPlugin || decodeMutation.isPending || plugins.length === 0}
          >
            {decodeMutation.isPending ? "解码中…" : "用插件解码"}
          </Button>
          {decodeMutation.isSuccess && decodeMutation.data && decodeMutation.data.decoded > 0 && onDecoded && (
            <Button variant="link" size="sm" className="h-auto p-0" onClick={onDecoded}>
              查看协议数据 →
            </Button>
          )}
        </div>
      )}

      {/* 统计信息 */}
      <div className="flex items-center justify-between px-1 text-xs text-muted-foreground" aria-live="polite">
        <span className="tabular-nums">
          共 {count} 条 · 显示第 {offset + 1}–{Math.min(offset + PAGE_SIZE, count)} 条
          {isPlaceholderData ? " · 更新中…" : ""}
        </span>
      </div>

      {/* 数据表格 */}
      <Table className="gta-table">
        <TableHeader>
          <TableRow>
            <TableHead className="w-44">Timestamp</TableHead>
            <TableHead className="w-32">Src</TableHead>
            <TableHead className="w-32">Dst</TableHead>
            <TableHead className="w-16">Proto</TableHead>
            <TableHead className="w-20">Len</TableHead>
            <TableHead>Payload (hex)</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {packets.map((pkt) => (
            <RawPacketRow
              key={pkt.id}
              pkt={pkt}
              isExpanded={expandedId === pkt.id}
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
