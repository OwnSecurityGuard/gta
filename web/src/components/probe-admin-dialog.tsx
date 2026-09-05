import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import {
  useListProbes,
  useProbeStopCapture,
  useProbeRetryCapture,
  useProbeRename,
  useProbeRevoke,
  useProbeListArchive,
  useProbeImportArchive,
} from "@/hooks/use-mcp";
import { toast } from "@/components/ui/toast";
import type { ProbeInfo } from "@/types/probe";
import {
  X,
  Server,
  Square,
  RotateCcw,
  Pencil,
  ShieldBan,
  Database,
  Download,
  RefreshCw,
  Loader2,
} from "lucide-react";

interface ProbeAdminDialogProps {
  open: boolean;
  onClose: () => void;
  /** 从归档导入产生新会话后回调（前端选中该会话）。 */
  onImported?: (sessionId: string) => void;
}

function formatBytes(n: number): string {
  if (!n) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
}

function formatUnix(sec: number): string {
  if (!sec) return "—";
  return new Date(sec * 1000).toLocaleString();
}

/** 三维度状态摘要行（connection / capture / data）。 */
function ProbeStatusLine({ p }: { p: ProbeInfo }) {
  const conn = p.connection_state === "online";
  return (
    <div className="flex flex-wrap items-center gap-1.5 text-[11px]">
      <span
        className={`inline-flex items-center gap-1 rounded px-1.5 py-0.5 ${
          conn
            ? "bg-emerald-500/15 text-emerald-700 dark:text-emerald-300"
            : "bg-muted text-muted-foreground"
        }`}
      >
        {conn && <span className="gta-live-dot" />}
        {conn ? "在线" : "离线"}
      </span>
      <span className="rounded bg-muted px-1.5 py-0.5 text-muted-foreground">
        抓包：{p.capture_state || "idle"}
      </span>
      {p.status_error && (
        <span className="max-w-[240px] truncate rounded bg-destructive/10 px-1.5 py-0.5 text-destructive" title={p.status_error}>
          {p.status_error}
        </span>
      )}
      <span className="text-muted-foreground">
        {p.last_packet_unix_ms
          ? `最近包 ${formatUnix(Math.floor(p.last_packet_unix_ms / 1000))}`
          : "从未抓到包"}
      </span>
      {p.dropped > 0 && (
        <span className="rounded bg-amber-500/15 px-1.5 py-0.5 text-amber-700 dark:text-amber-300">
          丢弃 {p.dropped}
        </span>
      )}
    </div>
  );
}

/** 探针管理弹窗（管理 > 探针）：三维度状态、改名/吊销、停抓/重试、本地归档与离线导入。 */
export function ProbeAdminDialog({ open, onClose, onImported }: ProbeAdminDialogProps) {
  const { data: probesData, isLoading } = useListProbes();
  const probes = probesData?.probes ?? [];
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [renamingId, setRenamingId] = useState<string | null>(null);
  const [renameValue, setRenameValue] = useState("");
  const [confirmRevokeId, setConfirmRevokeId] = useState<string | null>(null);

  const stopCapture = useProbeStopCapture();
  const retry = useProbeRetryCapture();
  const rename = useProbeRename();
  const revoke = useProbeRevoke();
  const importArchive = useProbeImportArchive();
  // 归档展开时查询该探针留存段（带 refresh 实时刷新，探针离线自动落缓存）。
  const { data: archiveData, isFetching: archiveFetching, refetch: refetchArchive } =
    useProbeListArchive(expandedId, { refresh: true });
  const segments = archiveData?.segments ?? [];

  function handleStop(p: ProbeInfo) {
    stopCapture.mutate(
      { probeId: p.probe_id },
      {
        onSuccess: (d) => {
          toast.success("抓包已停止", d?.session_id ? `会话 ${d.session_id}` : p.name);
        },
        onError: (err) => toast.error("停止失败", err.message),
      },
    );
  }

  function handleRetry(p: ProbeInfo) {
    retry.mutate(
      { probeId: p.probe_id },
      {
        onSuccess: (d) => {
          if (d?.ok) toast.success("已重试", p.name);
          else toast.error("重试失败", "探针没有可重试的抓包指派");
        },
        onError: (err) => toast.error("重试失败", err.message),
      },
    );
  }

  function handleRenameSubmit(p: ProbeInfo) {
    const name = renameValue.trim();
    if (!name) return;
    rename.mutate(
      { probeId: p.probe_id, name },
      {
        onSuccess: () => {
          setRenamingId(null);
          toast.success("已改名", name);
        },
        onError: (err) => toast.error("改名失败", err.message),
      },
    );
  }

  function handleRevoke(p: ProbeInfo) {
    revoke.mutate(
      { probeId: p.probe_id },
      {
        onSuccess: () => {
          setConfirmRevokeId(null);
          toast.success("凭证已吊销", `${p.name} 下次启动需重新接入`);
        },
        onError: (err) => toast.error("吊销失败", err.message),
      },
    );
  }

  function handleImport(p: ProbeInfo) {
    importArchive.mutate(
      { probeId: p.probe_id },
      {
        onSuccess: (d) => {
          if (d?.session_id) {
            toast.success("离线导入已开始", `新会话 ${d.session_id}，回放完成后可在列表查看`);
            onImported?.(d.session_id);
          }
        },
        onError: (err) => toast.error("导入失败", err.message),
      },
    );
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      icon={<Server className="h-5 w-5" />}
      title="探针"
      description="探针是抓包基础设施：接在成员机上，由会话驱动抓包。本地留存默认 24h / 4GB，可离线导入为会话。"
      footer={
        <Button variant="outline" onClick={onClose}>
          <X className="h-4 w-4" />
          关闭
        </Button>
      }
    >
      <div className="space-y-2">
        {isLoading ? (
          <div className="flex items-center justify-center gap-2 py-8 text-sm text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin" />
            加载中…
          </div>
        ) : probes.length === 0 ? (
          <div className="rounded-lg border border-dashed border-border bg-muted/40 px-3 py-6 text-center text-sm text-muted-foreground">
            还没有探针接入。通过顶部「接入设备」生成启动码，在成员机上运行 gta-agent 完成接入。
          </div>
        ) : (
          probes.map((p) => {
            const expanded = expandedId === p.probe_id;
            const capturing = p.capture_state === "running" || p.capture_state === "starting";
            return (
              <div key={p.probe_id} className="rounded-lg border border-border bg-background">
                <div className="flex items-center gap-2 px-3 py-2.5">
                  <Server className="h-4 w-4 shrink-0 text-muted-foreground" />
                  <div className="min-w-0 flex-1">
                    {renamingId === p.probe_id ? (
                      <div className="flex items-center gap-1.5">
                        <Input
                          value={renameValue}
                          onChange={(e) => setRenameValue(e.target.value)}
                          autoFocus
                          className="h-7 text-sm"
                          onKeyDown={(e) => {
                            if (e.key === "Enter") handleRenameSubmit(p);
                            if (e.key === "Escape") setRenamingId(null);
                          }}
                        />
                        <Button size="sm" className="h-7" onClick={() => handleRenameSubmit(p)}>
                          保存
                        </Button>
                        <Button size="sm" variant="outline" className="h-7" onClick={() => setRenamingId(null)}>
                          取消
                        </Button>
                      </div>
                    ) : (
                      <>
                        <div className="flex items-center gap-2">
                          <span className="truncate text-sm font-medium">{p.name}</span>
                          <span className="truncate font-mono text-[10px] text-muted-foreground">
                            {p.hostname} · {p.os}/{p.arch} · v{p.version || "?"}
                          </span>
                        </div>
                        <ProbeStatusLine p={p} />
                      </>
                    )}
                  </div>
                  <div className="flex shrink-0 items-center gap-1">
                    {capturing && (
                      <Button
                        size="sm"
                        variant="outline"
                        className="h-7"
                        onClick={() => handleStop(p)}
                        disabled={stopCapture.isPending}
                        title="停止该探针当前抓包并结束会话"
                      >
                        <Square className="h-3 w-3" />
                        停止
                      </Button>
                    )}
                    {p.capture_state === "failed" && (
                      <Button
                        size="sm"
                        variant="outline"
                        className="h-7"
                        onClick={() => handleRetry(p)}
                        disabled={retry.isPending}
                        title="重试上一次失败的抓包指派"
                      >
                        <RotateCcw className="h-3 w-3" />
                        重试
                      </Button>
                    )}
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-7 w-7 px-0"
                      onClick={() => {
                        setRenamingId(renamingId === p.probe_id ? null : p.probe_id);
                        setRenameValue(p.name);
                      }}
                      title="改名"
                    >
                      <Pencil className="h-3.5 w-3.5" />
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-7 w-7 px-0"
                      onClick={() => setExpandedId(expanded ? null : p.probe_id)}
                      title={p.archive_segments > 0 ? `本地留存 ${formatBytes(p.archive_bytes)}` : "本地留存"}
                    >
                      <Database className={`h-3.5 w-3.5 ${expanded ? "text-primary" : ""}`} />
                    </Button>
                    {confirmRevokeId === p.probe_id ? (
                      <>
                        <Button
                          size="sm"
                          variant="destructive"
                          className="h-7"
                          onClick={() => handleRevoke(p)}
                          disabled={revoke.isPending}
                        >
                          确认吊销
                        </Button>
                        <Button size="sm" variant="outline" className="h-7" onClick={() => setConfirmRevokeId(null)}>
                          取消
                        </Button>
                      </>
                    ) : (
                      <Button
                        size="sm"
                        variant="ghost"
                        className="h-7 w-7 px-0"
                        onClick={() => setConfirmRevokeId(p.probe_id)}
                        title="吊销凭证（探针下次启动需重新接入）"
                      >
                        <ShieldBan className="h-3.5 w-3.5 text-destructive" />
                      </Button>
                    )}
                  </div>
                </div>

                {expanded && (
                  <div className="border-t border-border px-3 py-2.5">
                    <div className="mb-2 flex items-center gap-2 text-xs text-muted-foreground">
                      <Database className="h-3.5 w-3.5" />
                      本地留存：{formatBytes(p.archive_bytes)} · {p.archive_segments} 段
                      {p.archive_oldest_unix > 0 && (
                        <>
                          {" "}
                          · {formatUnix(p.archive_oldest_unix)} ~ {formatUnix(p.archive_newest_unix)}
                        </>
                      )}
                      <button
                        type="button"
                        className="ml-auto inline-flex items-center gap-1 hover:text-foreground"
                        onClick={() => void refetchArchive()}
                      >
                        <RefreshCw className={`h-3 w-3 ${archiveFetching ? "animate-spin" : ""}`} />
                        刷新
                      </button>
                    </div>
                    {segments.length === 0 ? (
                      <p className="text-xs text-muted-foreground">
                        {archiveData?.from_cache ? "缓存中没有留存段。" : "该探针暂无本地留存数据。"}
                      </p>
                    ) : (
                      <div className="max-h-40 space-y-1 overflow-auto gta-scroll">
                        {segments.map((s) => (
                          <div
                            key={s.seg_id}
                            className="flex items-center gap-2 rounded bg-muted/50 px-2 py-1 font-mono text-[11px]"
                          >
                            <span className="truncate">{s.seg_id}</span>
                            <span className="ml-auto shrink-0 text-muted-foreground">
                              {formatUnix(s.first_unix)} ~ {formatUnix(s.last_unix)} · {s.packets} 包 ·{" "}
                              {formatBytes(s.bytes)}
                            </span>
                          </div>
                        ))}
                      </div>
                    )}
                    <div className="mt-2 flex justify-end">
                      <Button
                        size="sm"
                        variant="outline"
                        className="h-7"
                        disabled={p.connection_state !== "online" || importArchive.isPending}
                        title={p.connection_state !== "online" ? "探针离线，无法回放导入" : "把本地留存全部回放导入为新会话"}
                        onClick={() => handleImport(p)}
                      >
                        <Download className="h-3 w-3" />
                        导入为新会话
                      </Button>
                    </div>
                  </div>
                )}
              </div>
            );
          })
        )}
      </div>
    </Dialog>
  );
}
