import { useMemo, useState, Fragment } from "react";
import {
  useConnectionDetail,
  useConnectionStreams,
  useConnectionFrames,
} from "@/hooks/use-mcp";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import {
  Table,
  TableHeader,
  TableBody,
  TableRow,
  TableHead,
  TableCell,
} from "@/components/ui/table";
import {
  ArrowLeft,
  ArrowRight,
  Cable,
  Clock,
  ChevronDown,
  Rows3,
  Network,
  ListTree,
  Braces,
  FileText,
  Smartphone,
  Server,
  RotateCw,
} from "lucide-react";
import { unpackJsonStrings } from "@/lib/utils";
import { formatDuration, protocolLabel } from "@/components/connections-page";
import type {
  ConnectionDetail,
  ConnectionEvent,
  ConnectionFrame,
  ConnectionStream,
} from "@/types/connection";

interface ConnectionDetailViewProps {
  sessionId: string | null;
  connId: string;
  /** 连接列表中的序号（用于展示 Connection #001） */
  connSeq: number;
  onBack: () => void;
  /** 点击 flow_id 后跳转到「行为」Tab 并预填 flow_id 构建行为链 */
  onJumpToRun?: (flowId: string) => void;
}

type DetailTab = "timeline" | "streams" | "frames" | "events" | "raw";

const TABS: { id: DetailTab; label: string; icon: typeof Clock }[] = [
  { id: "timeline", label: "时间线", icon: Clock },
  { id: "streams", label: "流", icon: Rows3 },
  { id: "frames", label: "帧", icon: ListTree },
  { id: "events", label: "事件", icon: Braces },
  { id: "raw", label: "原始", icon: FileText },
];

/** 方向徽标（与 event-table 语义一致）。 */
function DirectionBadge({ direction }: { direction: string }) {
  if (direction === "client_to_server") {
    return (
      <span className="inline-flex items-center gap-0.5 rounded-full bg-blue-50 text-blue-700 px-2 py-0.5 text-xs font-medium dark:bg-blue-950 dark:text-blue-300">
        <ArrowRight className="h-3 w-3" />
        C→S
      </span>
    );
  }
  if (direction === "server_to_client") {
    return (
      <span className="inline-flex items-center gap-0.5 rounded-full bg-emerald-50 text-emerald-700 px-2 py-0.5 text-xs font-medium dark:bg-emerald-950 dark:text-emerald-300">
        <ArrowLeft className="h-3 w-3" />
        S→C
      </span>
    );
  }
  return (
    <span className="inline-flex items-center rounded-full bg-muted text-muted-foreground px-2 py-0.5 text-xs">
      ?
    </span>
  );
}

/** 时间（仅时分秒，用于流内紧凑展示）。 */
function formatTime(iso: string): string {
  try {
    return new Date(iso).toLocaleTimeString("zh-CN", { hour12: false });
  } catch {
    return iso;
  }
}

/** 时间（含日期，用于 Timeline/Frames/Events）。 */
function formatDateTime(iso: string): string {
  try {
    return new Date(iso).toLocaleString("zh-CN", {
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    });
  } catch {
    return iso;
  }
}

/** base64 → 字节数组。 */
function base64ToBytes(b64: string): Uint8Array {
  const binary = atob(b64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
}

/** 字节 → hex dump（偏移 | hex | ascii）。 */
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
        hexParts.push(chunk[i]!.toString(16).padStart(2, "0"));
        const ch = chunk[i]!;
        asciiParts.push(ch >= 0x20 && ch < 0x7f ? String.fromCharCode(ch) : ".");
      } else {
        hexParts.push("  ");
        asciiParts.push(" ");
      }
    }
    const offsetStr = offset.toString(16).padStart(8, "0");
    const hexStr = hexParts.slice(0, 8).join(" ") + "  " + hexParts.slice(8).join(" ");
    lines.push(`${offsetStr}  ${hexStr}  |${asciiParts.join("")}|`);
  }
  if (truncated) {
    lines.push(`... (${bytes.length} bytes total, showing first ${maxBytes})`);
  }
  return lines.join("\n");
}

/** JSON 视图（去转义 + 等宽展示）。 */
function JsonView({ data }: { data: Record<string, unknown> }) {
  const formatted = useMemo(() => JSON.stringify(unpackJsonStrings(data), null, 2), [data]);
  return (
    <pre className="gta-json-pre max-h-[400px] overflow-auto whitespace-pre text-xs font-mono">
      {formatted}
    </pre>
  );
}

// ─── 子页：Timeline ────────────────────────────────────────────

function TimelineTab({ streams }: { streams: ConnectionStream[] }) {
  // 摊平所有流的事件并按时间正序，形成纵向时间线。
  const events = useMemo(() => {
    const all: (ConnectionEvent & { streamSeq: number })[] = [];
    for (const st of streams) {
      for (const ev of st.events) {
        all.push({ ...ev, streamSeq: st.seq });
      }
    }
    all.sort((a, b) => a.timestamp.localeCompare(b.timestamp));
    return all;
  }, [streams]);

  if (events.length === 0) {
    return (
      <EmptyState
        icon={<Clock className="h-5 w-5" />}
        title="时间线为空"
        hint="该连接暂无解码事件。"
        className="h-48 justify-center"
      />
    );
  }

  return (
    <div className="relative ml-2 border-l border-border pl-6">
      {events.map((ev) => (
        <div key={ev.id} className="relative pb-4">
          <span className="absolute -left-[31px] top-1.5 h-2.5 w-2.5 rounded-full bg-primary ring-4 ring-background" />
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-mono text-xs tabular-nums text-muted-foreground whitespace-nowrap">
              {formatDateTime(ev.timestamp)}
            </span>
            <DirectionBadge direction={ev.direction} />
            <Badge variant="secondary" className="font-mono text-[10px]">
              Stream #{ev.streamSeq}
            </Badge>
            <span className="font-mono text-xs font-semibold truncate">
              {ev.msg_name || ev.type || "(unknown)"}
            </span>
          </div>
        </div>
      ))}
    </div>
  );
}

// ─── 子页：Streams（Stream View）──────────────────────────────

function StreamsTab({
  streams,
  onJumpToRun,
}: {
  streams: ConnectionStream[];
  onJumpToRun?: (flowId: string) => void;
}) {
  if (streams.length === 0) {
    return (
      <EmptyState
        icon={<Rows3 className="h-5 w-5" />}
        title="暂无流"
        hint="该连接尚未产生解码事件，无法划分流。"
        className="h-48 justify-center"
      />
    );
  }

  return (
    <div className="space-y-4">
      {streams.map((stream) => (
        <div
          key={stream.key}
          className="rounded-lg border border-border bg-card/60 p-4 gta-fade-in"
        >
          {/* 流头 */}
          <div className="mb-3 flex flex-wrap items-center gap-2">
            <Badge variant="default" className="font-mono">
              Stream #{stream.seq}
            </Badge>
            {stream.correlation_id && (
              <Badge variant="outline" className="font-mono text-[10px]" title="关联对话 ID">
                correlation: {stream.correlation_id}
              </Badge>
            )}
            <span className="text-xs text-muted-foreground tabular-nums">
              {formatDateTime(stream.start_time)}
              {stream.end_time !== stream.start_time && ` → ${formatDateTime(stream.end_time)}`}
            </span>
            <span className="text-xs text-muted-foreground tabular-nums">
              {stream.event_count} 个事件
            </span>
          </div>

          {/* 流内事件 */}
          <div className="space-y-1.5">
            {stream.events.map((ev) => (
              <div
                key={ev.id}
                className="flex flex-wrap items-center gap-2 rounded-md bg-muted/40 px-3 py-1.5"
              >
                <span className="font-mono text-xs tabular-nums text-muted-foreground whitespace-nowrap">
                  {formatTime(ev.timestamp)}
                </span>
                <DirectionBadge direction={ev.direction} />
                <span className="font-mono text-xs font-semibold truncate" title={ev.msg_name}>
                  {ev.msg_name || ev.type || "(unknown)"}
                </span>
                {ev.flow_id && (
                  <button
                    type="button"
                    className="ml-auto font-mono text-[10px] text-muted-foreground/70 truncate transition-colors hover:text-primary hover:underline"
                    title={`构建行为链 flow_id=${ev.flow_id}`}
                    onClick={() => onJumpToRun?.(ev.flow_id)}
                  >
                    flow: {ev.flow_id}
                  </button>
                )}
              </div>
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}

// ─── 子页：Frames ─────────────────────────────────────────────

function FrameHexRow({ payload }: { payload: string }) {
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
        <pre className="text-xs font-mono whitespace-pre overflow-x-auto">{hex}</pre>
      </TableCell>
    </TableRow>
  );
}

/** Raw 子页：帧的连续 hex dump 流（hex 计算 memo 化，避免轮询重复计算）。 */
function RawFrames({ frames }: { frames: ConnectionFrame[] }) {
  const hexes = useMemo(() => {
    const map: Record<string, string> = {};
    for (const f of frames) {
      map[f.id] = frameHex(f);
    }
    return map;
  }, [frames]);

  return (
    <div className="space-y-3">
      {frames.map((frame) => (
        <div key={frame.id} className="rounded-lg border border-border bg-card/60 p-3 gta-fade-in">
          <div className="mb-2 flex flex-wrap items-center gap-2 text-xs">
            <span className="font-mono tabular-nums text-muted-foreground">
              {formatDateTime(frame.timestamp)}
            </span>
            <DirectionBadge direction={frame.direction} />
            <span className="font-mono text-muted-foreground">
              {frame.src} → {frame.dst}
            </span>
            <span className="ml-auto font-mono tabular-nums text-muted-foreground">
              {frame.payload ? byteLen(frame.payload) : 0} B
            </span>
          </div>
          <pre className="text-xs font-mono whitespace-pre overflow-x-auto max-h-[300px] overflow-y-auto">
            {hexes[frame.id] ?? ""}
          </pre>
        </div>
      ))}
    </div>
  );
}

function FramesTab({
  sessionId,
  connId,
  rawOnly,
}: {
  sessionId: string | null;
  connId: string;
  /** rawOnly=true 时直接以纯 hex dump 展示（Raw 子页）。 */
  rawOnly?: boolean;
}) {
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const { data, isLoading, isError, error, refetch } = useConnectionFrames(sessionId, connId, {
    limit: 500,
    offset: 0,
  });
  const frames = useMemo(() => data?.frames ?? [], [data]);

  if (isLoading) {
    return (
      <div className="space-y-2 p-4">
        {Array.from({ length: 3 }).map((_, i) => (
          <Skeleton key={i} className="h-10 w-full" />
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

  if (frames.length === 0) {
    return (
      <EmptyState
        icon={<ListTree className="h-5 w-5" />}
        title="暂无帧"
        hint="该连接尚无原始帧记录。"
        className="h-48 justify-center"
      />
    );
  }

  return (
    <div className="space-y-3">
      {/* Raw 子页：连续 hex dump 流 */}
      {rawOnly ? (
        <RawFrames frames={frames} />
      ) : (
        <Table className="gta-table">
          <TableHeader>
            <TableRow>
              <TableHead className="w-40">时间</TableHead>
              <TableHead className="w-24">方向</TableHead>
              <TableHead>源 → 目标</TableHead>
              <TableHead className="w-20">协议</TableHead>
              <TableHead className="w-20 text-right">大小</TableHead>
              <TableHead className="w-10" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {frames.map((frame: ConnectionFrame) => (
              <Fragment key={frame.id}>
                <TableRow
                  className="cursor-pointer hover:bg-muted/50 transition-colors"
                  onClick={() => setExpandedId((prev) => (prev === frame.id ? null : frame.id))}
                  aria-expanded={expandedId === frame.id}
                >
                  <TableCell className="font-mono text-xs whitespace-nowrap">
                    {formatDateTime(frame.timestamp)}
                  </TableCell>
                  <TableCell>
                    <DirectionBadge direction={frame.direction} />
                  </TableCell>
                  <TableCell className="font-mono text-xs max-w-[300px]">
                    <span className="truncate block" title={`${frame.src} → ${frame.dst}`}>
                      {frame.src} → {frame.dst}
                    </span>
                  </TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground uppercase">
                    {frame.protocol || "-"}
                  </TableCell>
                  <TableCell className="text-right text-xs tabular-nums text-muted-foreground">
                    {frame.payload ? byteLen(frame.payload) : 0}
                  </TableCell>
                  <TableCell className="w-10 text-muted-foreground">
                    <ChevronDown className="h-4 w-4" />
                  </TableCell>
                </TableRow>
                {expandedId === frame.id && <FrameHexRow payload={frame.payload} />}
              </Fragment>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
}

function byteLen(b64: string): number {
  try {
    return base64ToBytes(b64).length;
  } catch {
    return 0;
  }
}

function frameHex(frame: ConnectionFrame): string {
  if (!frame.payload) return "(empty)";
  try {
    return hexDump(base64ToBytes(frame.payload), 8192);
  } catch {
    return "(decode error)";
  }
}

// ─── 子页：Events ─────────────────────────────────────────────

function EventsTab({
  streams,
  onJumpToRun,
}: {
  streams: ConnectionStream[];
  onJumpToRun?: (flowId: string) => void;
}) {
  const [expandedId, setExpandedId] = useState<string | null>(null);

  // 摊平全部流的事件，按时间正序，作为连接内的事件列表。
  const events = useMemo(() => {
    const all: (ConnectionEvent & { streamSeq: number })[] = [];
    for (const st of streams) {
      for (const ev of st.events) {
        all.push({ ...ev, streamSeq: st.seq });
      }
    }
    all.sort((a, b) => a.timestamp.localeCompare(b.timestamp));
    return all;
  }, [streams]);

  if (events.length === 0) {
    return (
      <EmptyState
        icon={<Braces className="h-5 w-5" />}
        title="暂无事件"
        hint="该连接尚未产生解码事件。"
        className="h-48 justify-center"
      />
    );
  }

  return (
    <div className="space-y-2">
      {events.map((ev) => (
        <div key={ev.id} className="rounded-md border border-border bg-card/40 overflow-hidden">
          <button
            type="button"
            className="flex w-full flex-wrap items-center gap-2 px-3 py-2 text-left hover:bg-muted/50 transition-colors"
            onClick={() => setExpandedId((prev) => (prev === ev.id ? null : ev.id))}
            aria-expanded={expandedId === ev.id}
          >
            <span className="font-mono text-xs tabular-nums text-muted-foreground whitespace-nowrap">
              {formatDateTime(ev.timestamp)}
            </span>
            <DirectionBadge direction={ev.direction} />
            <Badge variant="secondary" className="font-mono text-[10px]">
              Stream #{ev.streamSeq}
            </Badge>
            <span className="font-mono text-xs font-semibold truncate">
              {ev.msg_name || ev.type || "(unknown)"}
            </span>
            {ev.flow_id && (
              <span
                role="button"
                tabIndex={0}
                className="ml-auto font-mono text-[10px] text-muted-foreground/70 hover:text-primary hover:underline cursor-pointer"
                title={`构建行为链 flow_id=${ev.flow_id}`}
                onClick={(e) => {
                  e.stopPropagation();
                  onJumpToRun?.(ev.flow_id);
                }}
                onKeyDown={(e) => {
                  if (e.key === "Enter" || e.key === " ") {
                    e.preventDefault();
                    e.stopPropagation();
                    onJumpToRun?.(ev.flow_id);
                  }
                }}
              >
                flow: {ev.flow_id}
              </span>
            )}
          </button>
          {expandedId === ev.id && (
            <div className="border-t border-border bg-muted/30 p-4 gta-fade-in">
              <JsonView data={ev.data} />
            </div>
          )}
        </div>
      ))}
    </div>
  );
}

// ─── 详情头 ───────────────────────────────────────────────────

function DetailHeader({
  detail,
  connSeq,
}: {
  detail: ConnectionDetail;
  connSeq: number;
}) {
  return (
    <div className="rounded-lg border border-border bg-card/60 p-4 gta-fade-in">
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <h2 className="font-mono text-sm font-semibold">
          Connection #{String(connSeq).padStart(3, "0")}
        </h2>
        <Badge variant="secondary" className="font-mono">
          {protocolLabel(detail)}
        </Badge>
        {detail.source && (
          <Badge variant="outline" className="text-xs">
            {detail.source === "mobile" ? "Mobile Proxy" : detail.source}
          </Badge>
        )}
        <span className="ml-auto flex items-center gap-1.5 text-xs text-muted-foreground tabular-nums">
          <Clock className="h-3.5 w-3.5" />
          {formatDuration(detail.duration_sec)}
        </span>
      </div>

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {/* Client */}
        <div className="rounded-md bg-muted/40 p-3">
          <div className="mb-1 flex items-center gap-1.5 text-[11px] uppercase tracking-wide text-muted-foreground">
            <Smartphone className="h-3 w-3" />
            Client
          </div>
          <p className="font-mono text-sm truncate" title={detail.client}>
            {detail.client || "-"}
          </p>
          {detail.device && (
            <p className="mt-0.5 text-[11px] text-muted-foreground truncate">{detail.device}</p>
          )}
        </div>

        {/* Server */}
        <div className="rounded-md bg-muted/40 p-3">
          <div className="mb-1 flex items-center gap-1.5 text-[11px] uppercase tracking-wide text-muted-foreground">
            <Server className="h-3 w-3" />
            Server
          </div>
          <p className="font-mono text-sm truncate" title={detail.server}>
            {detail.server || "-"}
          </p>
          {detail.app && (
            <p className="mt-0.5 text-[11px] text-muted-foreground truncate">{detail.app}</p>
          )}
        </div>

        {/* Source */}
        <div className="rounded-md bg-muted/40 p-3">
          <div className="mb-1 flex items-center gap-1.5 text-[11px] uppercase tracking-wide text-muted-foreground">
            <Cable className="h-3 w-3" />
            Source
          </div>
          <p className="text-sm truncate">{detail.source === "mobile" ? "Mobile Proxy" : detail.source || "-"}</p>
        </div>

        {/* 统计 */}
        <div className="rounded-md bg-muted/40 p-3">
          <div className="mb-1 flex items-center gap-1.5 text-[11px] uppercase tracking-wide text-muted-foreground">
            <Network className="h-3 w-3" />
            Stats
          </div>
          <p className="text-sm tabular-nums">
            {detail.event_count} 事件 · {detail.stream_count} 流 · {detail.frame_count} 帧
          </p>
        </div>
      </div>
    </div>
  );
}

// ─── 主组件 ───────────────────────────────────────────────────

export function ConnectionDetailView({
  sessionId,
  connId,
  connSeq,
  onBack,
  onJumpToRun,
}: ConnectionDetailViewProps) {
  const [tab, setTab] = useState<DetailTab>("timeline");

  const {
    data: detailData,
    isLoading,
    isError,
    error,
    refetch,
  } = useConnectionDetail(sessionId, connId);
  const { data: streamsData, isLoading: streamsLoading } = useConnectionStreams(
    sessionId,
    connId,
    { limit: 500, offset: 0 },
  );

  const detail = detailData?.connection;
  const streams = useMemo(() => streamsData?.streams ?? [], [streamsData]);

  return (
    <div className="space-y-4">
      {/* 返回按钮 */}
      <Button variant="ghost" size="sm" className="-ml-2 h-7 gap-1" onClick={onBack}>
        <ArrowLeft className="h-4 w-4" />
        返回连接列表
      </Button>

      {/* 详情头 */}
      {isLoading && <Skeleton className="h-36 w-full" />}
      {isError && (
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
      )}
      {!isLoading && !isError && detail && <DetailHeader detail={detail} connSeq={connSeq} />}

      {/* 子页 Tab */}
      <div
        role="tablist"
        aria-label="连接详情视图"
        className="flex items-center gap-1 rounded-lg bg-muted p-1"
      >
        {TABS.map((t) => {
          const selected = tab === t.id;
          const Icon = t.icon;
          return (
            <button
              key={t.id}
              role="tab"
              aria-selected={selected}
              onClick={() => setTab(t.id)}
              className={
                "inline-flex items-center gap-1.5 rounded-md px-3 py-1.5 text-sm font-medium transition-[background-color,color,box-shadow] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40 " +
                (selected
                  ? "bg-card text-foreground shadow-sm"
                  : "text-muted-foreground hover:text-foreground")
              }
            >
              <Icon className="h-3.5 w-3.5" />
              {t.label}
            </button>
          );
        })}
      </div>

      {/* 子页内容 */}
      <div className="min-h-[200px]">
        {streamsLoading && tab !== "frames" && (
          <div className="space-y-2 p-4">
            {Array.from({ length: 3 }).map((_, i) => (
              <Skeleton key={i} className="h-10 w-full" />
            ))}
          </div>
        )}
        {!streamsLoading && tab === "timeline" && <TimelineTab streams={streams} />}
        {!streamsLoading && tab === "streams" && (
          <StreamsTab streams={streams} onJumpToRun={onJumpToRun} />
        )}
        {tab === "frames" && <FramesTab sessionId={sessionId} connId={connId} />}
        {!streamsLoading && tab === "events" && (
          <EventsTab streams={streams} onJumpToRun={onJumpToRun} />
        )}
        {tab === "raw" && <FramesTab sessionId={sessionId} connId={connId} rawOnly />}
      </div>
    </div>
  );
}
