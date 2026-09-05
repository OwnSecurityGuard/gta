import { useState, useEffect } from "react";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import {
  useStartCapture,
  useBeginCaptureRun,
  useRegisteredPlugins,
  useListInterfaces,
  useSessionStatus,
  useListProbes,
  useProbeStartCapture,
} from "@/hooks/use-mcp";
import { groupParsers, GROUP_LABEL } from "@/lib/parsers";
import { toast } from "@/components/ui/toast";
import type { ProbeInfo } from "@/types/probe";
import { X, Check, Play, Network, ChevronDown, Loader2, Server, MonitorSmartphone } from "lucide-react";

interface StartCaptureDialogProps {
  open: boolean;
  onClose: () => void;
  onStarted?: (sessionId: string) => void;
  /** 抓包启动并自动开启行为窗口后回调，携带 run_id 与联动的 session_id。 */
  onRunLinked?: (runId: string, sessionId: string) => void;
  /** 从项目预填的默认端口（0=不预填） */
  initialPort?: number;
  /** 从项目预填的默认插件名 */
  initialPlugin?: string;
  /** 从项目一键抓包时绑定的项目 id；抓包会话归属到该项目 */
  initialProjectId?: string;
}

/** 探针选择卡片的可抓包判定：在线且不在抓包中（idle/stopped/failed 可选）。 */
function probeSelectable(p: ProbeInfo): boolean {
  if (p.connection_state !== "online") return false;
  return p.capture_state !== "starting" && p.capture_state !== "running";
}

function probeDisabledReason(p: ProbeInfo): string {
  if (p.connection_state !== "offline") {
    if (p.capture_state === "running" || p.capture_state === "starting") {
      return "抓包中";
    }
  }
  return "离线";
}

/** 探针三维度状态 chip（connection + capture 合并展示）。 */
function ProbeStateChip({ p }: { p: ProbeInfo }) {
  if (p.connection_state !== "online") {
    return (
      <span className="rounded bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
        离线
      </span>
    );
  }
  switch (p.capture_state) {
    case "running":
      return (
        <span className="inline-flex items-center gap-1 rounded bg-emerald-500/15 px-1.5 py-0.5 text-[10px] text-emerald-700 dark:text-emerald-300">
          <span className="gta-live-dot" />
          抓包中
        </span>
      );
    case "starting":
      return (
        <span className="rounded bg-amber-500/15 px-1.5 py-0.5 text-[10px] text-amber-700 dark:text-amber-300">
          启动中
        </span>
      );
    case "failed":
      return (
        <span className="rounded bg-destructive/15 px-1.5 py-0.5 text-[10px] text-destructive">
          失败
        </span>
      );
    default:
      return (
        <span className="rounded bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
          空闲
        </span>
      );
  }
}

/** 开始抓包对话框：本机网卡抓包 / 选一台探针机器抓包（探针是基础设施，管理见「探针」入口）。 */
export function StartCaptureDialog({
  open,
  onClose,
  onStarted,
  onRunLinked,
  initialPort,
  initialPlugin,
  initialProjectId,
}: StartCaptureDialogProps) {
  const [source, setSource] = useState<"nic" | "agent">("nic");
  const [probeId, setProbeId] = useState("");
  const [port, setPort] = useState("8080");
  const [plugin, setPlugin] = useState("");
  // 从项目一键抓包时带入的项目 id（本次抓包会话归属到此项目）。
  const [projectId, setProjectId] = useState("");
  const [started, setStarted] = useState(false);
  // 探针/agent 源启动成功后保持弹窗打开：轮询会话状态直到推流到达。
  const [agentSessionId, setAgentSessionId] = useState<string | null>(null);
  // 高级设置折叠：Interface/BPF 等技术细节默认收起，普通用户只看 端口 + 解析器。
  const [showAdvanced, setShowAdvanced] = useState(false);
  const start = useStartCapture();
  // 抓包成功后自动开启行为窗口：begin_capture_run 会读取 MCP 侧 current.json
  // （start_capture 已在服务端同步写入），从而复用同一 session_id，实现抓取↔窗口联动。
  const begin = useBeginCaptureRun();
  // 已注册且在线才能用于抓包解码；离线插件无法建立解码流，故置灰禁用但保留可见，便于排查。
  const { data: pluginsData } = useRegisteredPlugins();
  const plugins = pluginsData?.plugins ?? [];
  // list_interfaces：本机抓包的网卡列表（仅供参考——当前一次 MCP 服务实例只绑一个 -iface）。
  const { data: ifacesData } = useListInterfaces();
  const interfaces = ifacesData?.interfaces ?? [];
  // 已注册插件归组（Godot/Unity/HTTP/自定义），供普通用户以卡片而非下拉选择解析器。
  const pluginGroups = groupParsers(plugins);

  // 探针列表：agent 源 = 选一台有权限的探针机器（用户视角是"我要抓这台服务器"）。
  const { data: probesData } = useListProbes();
  const probes = probesData?.probes ?? [];
  const probeStart = useProbeStartCapture();

  // 等待推流期间 2s 轮询会话实时状态：packets_in/raw_count > 0 即探针已推流。
  const { data: agentLiveStatus } = useSessionStatus(agentSessionId, 2000);
  const agentPacketsIn =
    (agentLiveStatus?.packets_in ?? 0) + (agentLiveStatus?.raw_count ?? 0);
  const agentConnected = agentSessionId != null && agentPacketsIn > 0;
  const agentSessionClosed = agentSessionId != null && agentLiveStatus?.state === "closed";

  useEffect(() => {
    if (open) {
      setStarted(false);
      setAgentSessionId(null);
      setProbeId("");
      // 打开时应用项目预填：有初始端口/插件才覆盖默认值，否则回到默认。
      if (initialPort && initialPort > 0) setPort(String(initialPort));
      if (initialPlugin) setPlugin(initialPlugin);
      setProjectId(initialProjectId ?? "");
    }
    // 仅在每次打开时读取一次预填（把 initialPort/initialPlugin 当作当次快照）。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  // 探针推流到达提示（只弹一次：等待 → 已推流的边沿）。
  const [agentConnectedNotified, setAgentConnectedNotified] = useState(false);
  useEffect(() => {
    if (agentConnected && !agentConnectedNotified) {
      setAgentConnectedNotified(true);
      toast.success("数据已到达", "探针正在推流，可以开始分析");
    }
    if (agentSessionId == null) setAgentConnectedNotified(false);
  }, [agentConnected, agentConnectedNotified, agentSessionId]);

  function handleStart() {
    const p = parseInt(port, 10);
    // 本机网卡抓包要求端口（BPF 过滤用）；探针抓包由探针侧按端口过滤，可留空。
    if (source === "nic" && (!p || p <= 0)) return;
    if (source === "agent") {
      // 探针抓包：建会话 + AssignCapture 一体（probe_start_capture）。
      const target = probes.find((x) => x.probe_id === probeId);
      if (!target) {
        toast.error("请选择一台探针机器");
        return;
      }
      probeStart.mutate(
        {
          probeId,
          ports: p > 0 ? [p] : undefined,
          plugin: plugin || undefined,
          projectId: projectId || undefined,
        },
        {
          onSuccess: (data) => {
            const sessionId = data?.session_id ?? "";
            if (sessionId) onStarted?.(sessionId);
            toast.success("抓包已下发", `${target.name} · 会话 ${sessionId}`);
            // 进入「等待推流」闭环：探针收到指令开网卡 → 推流到达。
            setStarted(true);
            setAgentSessionId(sessionId);
          },
          onError: (err) => {
            toast.error("下发失败", err.message);
          },
        },
      );
      return;
    }
    start.mutate(
      {
        port: p > 0 ? p : 0,
        plugin: plugin || undefined,
        source,
        projectId: projectId || undefined,
      },
      {
        onSuccess: (data) => {
          const sessionId = data?.session_id ?? "";
          if (sessionId) onStarted?.(sessionId);
          const detail = [port ? `端口 ${port}` : null, plugin ? `插件 ${plugin}` : null]
            .filter(Boolean)
            .join(" · ");
          toast.success("抓包会话已启动", detail);
          const dbPath = data?.db_path ?? "";
          const sessionDir = dbPath.replace(/[\\/][^\\/]+$/, "") || `session-${sessionId}`;
          begin.mutate(
            {
              featureName: plugin ? `capture-${plugin}` : "capture",
              projectPath: sessionDir,
              pluginName: plugin || undefined,
              port: p,
              filter: `tcp port ${p}`,
            },
            {
              onSuccess: (runData) => {
                if (runData?.run_id) {
                  onRunLinked?.(runData.run_id, runData.session_id ?? sessionId);
                  toast.success("已开启行为窗口", `run ${runData.run_id}`);
                }
              },
              onError: (err) => {
                // 抓包已成功，行为窗口失败仅告警，不阻断抓包。
                toast.info("行为窗口开启失败（抓包仍在进行）", err.message);
              },
            },
          );
          setStarted(true);
          setTimeout(() => {
            setStarted(false);
            onClose();
          }, 800);
        },
      },
    );
  }

  // 统一关闭路径：清掉等待状态再回调（重新打开时 useEffect 亦会兜底重置）。
  function handleClose() {
    setAgentSessionId(null);
    onClose();
  }

  const selectedProbe = probes.find((x) => x.probe_id === probeId);

  return (
    <Dialog
      open={open}
      onClose={handleClose}
      icon={<Play className="h-5 w-5" />}
      title="开始抓包"
      description="本机网卡抓包，或选一台探针机器开始抓包；移动代理抓包为常驻服务，请在「代理服务器配置」中查看连接二维码。"
      footer={
        <>
          <Button variant="outline" onClick={handleClose}>
            <X className="h-4 w-4" />
            取消
          </Button>
          <Button
            onClick={handleStart}
            disabled={
              start.isPending ||
              probeStart.isPending ||
              (source === "agent" && (!probeId || !selectedProbe || !probeSelectable(selectedProbe)))
            }
          >
            {started ? (
              <>
                <Check className="h-4 w-4" />
                已启动
              </>
            ) : start.isPending || probeStart.isPending ? (
              "启动中…"
            ) : (
              "启动"
            )}
          </Button>
        </>
      }
    >
      {agentSessionId ? (
        // 探针/agent 源成功态：等待推流 → 已到达 的闭环展示。
        <div className="space-y-3">
          {agentSessionClosed ? (
            <div className="flex items-center gap-2 rounded-lg border border-border bg-muted/50 px-3 py-2.5 text-sm text-muted-foreground">
              <X className="h-4 w-4 shrink-0" />
              会话已结束（可能在探针侧被停止）。
            </div>
          ) : agentConnected ? (
            <div className="flex items-center gap-2 rounded-lg border border-emerald-500/40 bg-emerald-500/10 px-3 py-2.5 text-sm text-emerald-700 dark:text-emerald-300">
              <Check className="h-4 w-4 shrink-0" />
              探针正在推流
              <span className="ml-auto font-mono text-xs">
                {agentPacketsIn.toLocaleString()} packets ·{" "}
                {(agentLiveStatus?.event_count ?? 0).toLocaleString()} events
              </span>
            </div>
          ) : (
            <div className="flex items-center gap-2 rounded-lg border border-border bg-muted/50 px-3 py-2.5 text-sm">
              <Loader2 className="h-4 w-4 shrink-0 animate-spin text-primary" />
              指令已下发，等待探针开始推流…
              <span className="ml-auto text-xs text-muted-foreground">
                探针在线时会自动对齐并开抓
              </span>
            </div>
          )}
          <div className="flex items-center justify-end gap-2">
            <Button onClick={handleClose}>
              <Check className="h-4 w-4" />
              {agentConnected ? "进入会话分析" : "完成"}
            </Button>
          </div>
        </div>
      ) : (
        <div className="space-y-3">
          <div>
            <label className="text-sm font-medium">抓包源</label>
            <div
              role="radiogroup"
              aria-label="抓包源"
              className="mt-1.5 flex items-center gap-1 rounded-lg bg-muted p-1"
            >
              {(
                [
                  { id: "nic", label: "本机网卡" },
                  { id: "agent", label: "探针机器" },
                ] as const
              ).map((opt) => (
                <button
                  key={opt.id}
                  type="button"
                  role="radio"
                  aria-checked={source === opt.id}
                  onClick={() => setSource(opt.id)}
                  className={
                    "flex-1 rounded-md px-3 py-1.5 text-sm font-medium transition-[background-color,color] " +
                    (source === opt.id
                      ? "bg-card text-foreground shadow-sm"
                      : "text-muted-foreground hover:text-foreground")
                  }
                >
                  {opt.label}
                </button>
              ))}
            </div>
            {source === "agent" && (
              <p className="mt-1.5 text-xs text-muted-foreground">
                选择一台你有权限的探针机器，抓包会话将自动指派给它；探针的接入与留存在「探针」入口管理。
              </p>
            )}
          </div>

          {source === "agent" ? (
            <div>
              <label className="text-sm font-medium">选择机器</label>
              {probes.length === 0 ? (
                <div className="mt-1.5 rounded-lg border border-dashed border-border bg-muted/40 px-3 py-4 text-center text-xs text-muted-foreground">
                  还没有可用的探针机器。通过顶部「接入设备」生成启动码，
                  在目标机器上运行 gta-agent 完成接入。
                </div>
              ) : (
                <div className="mt-1.5 grid max-h-52 grid-cols-1 gap-1.5 overflow-auto gta-scroll">
                  {probes.map((p) => {
                    const selectable = probeSelectable(p);
                    const active = probeId === p.probe_id;
                    return (
                      <button
                        key={p.probe_id}
                        type="button"
                        aria-pressed={active}
                        disabled={!selectable}
                        onClick={() => setProbeId(p.probe_id)}
                        title={selectable ? p.hostname : probeDisabledReason(p)}
                        className={`flex items-center gap-2.5 rounded-md border px-2.5 py-2 text-left text-sm transition-colors ${
                          active
                            ? "border-primary/60 bg-primary/10 text-foreground"
                            : "border-border bg-background text-muted-foreground"
                        } ${selectable ? "cursor-pointer hover:bg-muted/60" : "cursor-not-allowed opacity-50"}`}
                      >
                        <Server className="h-4 w-4 shrink-0 text-muted-foreground" />
                        <span className="min-w-0 flex-1">
                          <span className="block truncate font-medium text-foreground">{p.name}</span>
                          <span className="block truncate font-mono text-[10px]">
                            {p.hostname} · {p.capture_iface || "自动选网卡"}
                          </span>
                        </span>
                        <ProbeStateChip p={p} />
                      </button>
                    );
                  })}
                </div>
              )}
            </div>
          ) : null}

          <div>
            <label className="text-sm font-medium">端口</label>
            <Input
              value={port}
              onChange={(e) => setPort(e.target.value)}
              aria-label="监听端口"
              inputMode="numeric"
              placeholder={source === "nic" ? "8080" : "可留空（全抓）"}
              className="mt-1.5 font-mono"
            />
          </div>
          {/* 高级设置（默认收起）：Interface 等技术细节，普通用户只需选端口 + 解析器。 */}
          <button
            type="button"
            onClick={() => setShowAdvanced((v) => !v)}
            aria-expanded={showAdvanced}
            className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
          >
            <ChevronDown className={`h-3.5 w-3.5 transition-transform ${showAdvanced ? "rotate-180" : ""}`} />
            高级设置
          </button>
          {showAdvanced && (
            <div>
              <label className="flex items-center gap-1.5 text-sm font-medium">
                {source === "nic" ? (
                  <>
                    <Network className="h-3.5 w-3.5 text-muted-foreground" />
                    可用网卡（参考）
                  </>
                ) : (
                  <>
                    <MonitorSmartphone className="h-3.5 w-3.5 text-muted-foreground" />
                    探针侧网卡
                  </>
                )}
              </label>
              {source === "nic" ? (
                interfaces.length === 0 ? (
                  <p className="mt-1 text-xs text-muted-foreground">暂无可列举的网卡。</p>
                ) : (
                  <div className="mt-1.5 flex flex-wrap gap-1.5">
                    {interfaces.map((iface) => (
                      <span
                        key={iface.name}
                        className="rounded-md border border-border bg-muted px-2 py-0.5 font-mono text-[11px] text-muted-foreground"
                        title={iface.name}
                      >
                        {iface.name}
                      </span>
                    ))}
                  </div>
                )
              ) : (
                <p className="mt-1 text-xs text-muted-foreground">
                  探针默认自动选择默认网卡；如需指定，稍后在「探针」管理里对该机器下发抓包时填写。
                </p>
              )}
              {source === "nic" && (
                <p className="mt-1 text-xs text-muted-foreground">
                  网卡在 MCP 启动时由 <code className="font-mono">-iface</code> 固定，本对话框不切换网卡。
                </p>
              )}
            </div>
          )}

          <div>
            <label className="text-sm font-medium">解码解析器（可选）</label>
            {pluginGroups.order.length === 0 ? (
              <p className="mt-1.5 text-xs text-muted-foreground">
                当前没有已注册的解析器，可留空仅抓包；或先启动解析器插件使其注册到 Pipeline。
              </p>
            ) : (
              <>
                <div className="mt-1.5 grid grid-cols-2 gap-1.5">
                  {pluginGroups.order.map((g) =>
                    pluginGroups.byGroup[g].map((opt) => (
                      <button
                        key={opt.plugin}
                        type="button"
                        aria-pressed={plugin === opt.plugin}
                        disabled={!opt.online}
                        onClick={() => setPlugin(plugin === opt.plugin ? "" : opt.plugin)}
                        className={`flex items-center gap-2 rounded-md border px-2.5 py-2 text-sm transition-colors ${
                          plugin === opt.plugin
                            ? "border-primary/60 bg-primary/10 text-foreground"
                            : "border-border bg-background text-muted-foreground"
                        } ${opt.online ? "cursor-pointer hover:bg-muted/60" : "cursor-not-allowed opacity-50"}`}
                      >
                        <span className="rounded bg-muted px-1 py-0.5 font-mono text-[10px] uppercase">
                          {GROUP_LABEL[g] ?? g}
                        </span>
                        <span className="truncate">{opt.label}</span>
                      </button>
                    )),
                  )}
                </div>
                <p className="mt-1 text-xs text-muted-foreground">
                  {plugin ? "已选择：留空为不指定（仅抓包）。" : "点击选择一个解析器；离线解析器置灰不可选。"}
                </p>
              </>
            )}
          </div>
        </div>
      )}

      {(start.isError || probeStart.isError) && (
        <p className="mt-3 text-xs text-destructive">
          启动失败：{probeStart.error?.message ?? start.error?.message}
        </p>
      )}
    </Dialog>
  );
}
