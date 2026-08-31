import { useEffect, useMemo, useState } from "react";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import {
  Download,
  Server,
  Radio,
  X,
  Monitor,
  Check,
  ExternalLink,
  Loader2,
} from "lucide-react";
import { useAgentDownloadOptions, useRegisteredPlugins, useSessionStatus } from "@/hooks/use-mcp";
import { AccessCodePanel } from "@/components/access-code-panel";
import { authHeaders } from "@/lib/auth";
import { toast } from "@/components/ui/toast";
import type { AgentPlatform } from "@/types/agent";

interface AgentDownloadDialogProps {
  open: boolean;
  onClose: () => void;
  /** 下载会话建立后跳转到该会话（自动选中 + 打开实时数据） */
  onNavigateToSession: (sessionId: string) => void;
}

/** 从 UA 尽力推断用户的操作系统/架构，用于默认推荐下载平台。 */
function detectPlatform(): { os: string; arch: string } {
  if (typeof navigator === "undefined") return { os: "windows", arch: "amd64" };
  const ua = navigator.userAgent;
  const os = /Mac/.test(ua) ? "darwin" : /Linux/.test(ua) ? "linux" : "windows";
  const uad = (navigator as { userAgentData?: { architecture?: string } }).userAgentData;
  const archHint = (uad?.architecture ?? "").toLowerCase();
  const arch =
    archHint.includes("arm") || /arm/i.test(ua)
      ? "arm64"
      : /x64|win64|amd64|_64/i.test(ua)
        ? "amd64"
        : "amd64";
  return { os, arch };
}

/**
 * 远程 Agent 下载对话框（Web First · 多平台）：
 * 为不在同一网络环境的成员生成「免参数」的抓包 agent。用户只需选目标平台 +
 * 抓包端口 + 解码插件，回连地址/端口/token/会话都打包进 zip（通用二进制 +
 * 运行时 sidecar 配置）。下载后进入「等待连接 → 已连接抓包」的生命周期，
 * 并提供到实时数据的入口。平台取自服务端预置产物，不再依赖服务端平台。
 */
export function AgentDownloadDialog({ open, onClose, onNavigateToSession }: AgentDownloadDialogProps) {
  const { data, isLoading } = useAgentDownloadOptions();
  const { data: pluginsData } = useRegisteredPlugins();
  const plugins = pluginsData?.plugins ?? [];

  const opts = (data ?? null) as null | NonNullable<typeof data>;
  const platforms = opts?.platforms ?? [];
  const available = platforms.filter((p) => p.available);

  // 阶段：configure（填写配置 + 下载）→ awaiting（等待 Agent 连接 / 抓包中）
  const [phase, setPhase] = useState<"configure" | "awaiting">("configure");
  const [sessionId, setSessionId] = useState<string | null>(null);
  // 接入模式：quick=启动码主路径（推荐） / advanced=原「下载 zip + sidecar 配置」
  const [mode, setMode] = useState<"quick" | "advanced">("quick");

  const [os, setOs] = useState("windows");
  const [arch, setArch] = useState("amd64");
  const [port, setPort] = useState("8080");
  const [plugin, setPlugin] = useState("");
  const [server, setServer] = useState("");
  const [busy, setBusy] = useState(false);

  // 默认回连 IP：优先取浏览器当前访问的服务端 hostname；取不到时退回服务端探测的 host。
  const deployHost = [
    typeof window !== "undefined" ? window.location.hostname : "",
    opts?.host ?? "",
  ].find((h) => Boolean(h)) ?? "";

  // 打开/拿到平台时：预填回连地址，并按用户 UA 推荐首个可用平台。
  useEffect(() => {
    if (!open) return;
    setBusy(false);
    setPhase("configure");
    setSessionId(null);
    setMode("quick");
    if (deployHost && opts?.registry_port) {
      setServer(`${deployHost}:${opts.registry_port}`);
    } else {
      setServer("");
    }
    if (available.length > 0) {
      const det = detectPlatform();
      const match = available.find((p) => p.os === det.os && p.arch === det.arch) ?? available[0];
      if (match) {
        setOs(match.os);
        setArch(match.arch);
      }
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, opts?.registry_port, deployHost]);

  // awaiting 阶段轮询会话实时状态，驱动「等待 → 已连接/抓包中」。
  const { data: status } = useSessionStatus(phase === "awaiting" ? sessionId : null);
  const live = useMemo(() => {
    if (!status) return { packets: 0, events: 0 };
    return {
      packets: status.packets_in ?? status.raw_count ?? status.raw_packets ?? 0,
      events: status.event_count ?? status.events ?? 0,
    };
  }, [status]);
  const attached = live.packets > 0;

  const selectedPlatform = platforms.find((p) => p.os === os && p.arch === arch);

  function defaultServer(): string {
    if (deployHost && opts?.registry_port) return `${deployHost}:${opts.registry_port}`;
    return "";
  }

  function validServer(value: string): boolean {
    if (!value.trim()) return false;
    const m = /^[^:\s]+:\d{1,5}$/.exec(value.trim());
    if (!m) return false;
    const p = Number(m[1]);
    return p >= 1 && p <= 65535;
  }

  async function handleDownload() {
    const p = Number(port.trim());
    if (!Number.isInteger(p) || p <= 0 || p > 65535) {
      toast.error("请填写有效的端口", "1-65535 之间");
      return;
    }
    if (!selectedPlatform) {
      toast.error("请选择操作系统", "当前没有任何可下载的平台产物");
      return;
    }
    const addr = server.trim() || defaultServer();
    if (!validServer(addr)) {
      toast.error("请填写有效的回连地址", "格式 host:port，如 192.168.1.10:9091");
      return;
    }
    if (!deployHost || !opts?.registry_port) {
      toast.error("服务端信息未就绪", "请稍后重试");
      return;
    }

    setBusy(true);
    try {
      // token 走请求头（authHeaders 带 Authorization: Bearer），不再进 URL。
      const url = `/download/agent?port=${encodeURIComponent(p)}&server=${encodeURIComponent(
        addr,
      )}&plugin=${encodeURIComponent(plugin)}&platform=${encodeURIComponent(
        `${selectedPlatform.os}/${selectedPlatform.arch}`,
      )}`;
      const resp = await fetch(url, { headers: authHeaders() });
      if (!resp.ok) {
        const txt = (await resp.text().catch(() => "")) || `HTTP ${resp.status}`;
        toast.error("Agent 下载失败", txt.slice(0, 200));
        return;
      }
      const newSessionId = resp.headers.get("X-Session-Id") ?? "";
      const blob = await resp.blob();
      const objUrl = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = objUrl;
      a.download = `gta-agent-${selectedPlatform.os}-${selectedPlatform.arch}.zip`;
      a.rel = "noopener";
      document.body.appendChild(a);
      a.click();
      a.remove();
      URL.revokeObjectURL(objUrl);

      if (newSessionId) {
        setSessionId(newSessionId);
        setPhase("awaiting");
        toast.success("Agent 已下载", `抓包端口 ${p}，等待 Agent 连接`);
        return;
      }
      toast.success("Agent 下载已触发");
    } catch (e) {
      toast.error("下载失败", e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  const inAdvanced = mode === "advanced";
  const dialogTitle =
    mode === "quick"
      ? "我的接入"
      : phase === "awaiting"
        ? "等待 Agent 连接"
        : "下载远程 Agent";
  const dialogDesc =
    mode === "quick"
      ? "生成一次性启动码，在目标机执行复制到的命令即可免参数注册并回连抓包。"
      : phase === "awaiting"
        ? "在目标电脑解压并双击运行 Agent，GTA 会自动回连并开始抓包。"
        : "选择目标操作系统与抓包端口后下载，回连地址、token 与会话都已打入 zip，运行即可免参数抓包上报。";

  return (
    <Dialog
      open={open}
      onClose={onClose}
      icon={<Download className="h-5 w-5" />}
      title={dialogTitle}
      description={dialogDesc}
      footer={
        inAdvanced && phase === "awaiting" ? (
          <>
            <Button variant="outline" onClick={() => setPhase("configure")}>
              <Download className="h-4 w-4" />
              下载另一个 Agent
            </Button>
            <Button
              variant="outline"
              onClick={() => onClose()}
              className="ml-auto"
            >
              <X className="h-4 w-4" />
              关闭
            </Button>
            <Button onClick={() => onNavigateToSession(sessionId ?? "")} disabled={!sessionId}>
              <ExternalLink className="h-4 w-4" />
              查看实时数据
            </Button>
          </>
        ) : inAdvanced ? (
          <>
            <Button variant="outline" onClick={onClose}>
              <X className="h-4 w-4" />
              关闭
            </Button>
            <Button onClick={handleDownload} disabled={busy || isLoading || !opts?.host}>
              <Download className="h-4 w-4" />
              {busy ? "打包下载中…" : "下载 Agent"}
            </Button>
          </>
        ) : (
          <>
            <Button variant="outline" onClick={onClose} className="ml-auto">
              <X className="h-4 w-4" />
              关闭
            </Button>
          </>
        )
      }
    >
      {/* 接入模式切换：启动码主路径（推荐） / 高级下载（zip + sidecar 配置） */}
      {!(inAdvanced && phase === "awaiting") && (
        <div className="mb-3 grid grid-cols-2 gap-1 rounded-md border border-border bg-muted/40 p-1">
          <button
            type="button"
            onClick={() => setMode("quick")}
            aria-pressed={mode === "quick"}
            className={`rounded px-2 py-1.5 text-sm font-medium transition-colors ${
              mode === "quick" ? "bg-background shadow-sm text-foreground" : "text-muted-foreground"
            }`}
          >
            ⚡ 快捷接入（推荐）
          </button>
          <button
            type="button"
            onClick={() => setMode("advanced")}
            aria-pressed={mode === "advanced"}
            className={`rounded px-2 py-1.5 text-sm font-medium transition-colors ${
              mode === "advanced" ? "bg-background shadow-sm text-foreground" : "text-muted-foreground"
            }`}
          >
            高级下载
          </button>
        </div>
      )}

      {mode === "quick" ? (
        <AccessCodePanel />
      ) : phase === "awaiting" ? (
        <AwaitingAgentPanel attached={attached} packets={live.packets} events={live.events} />
      ) : (
        <div className="space-y-3">
          {/* 目标平台 */}
          <div>
            <label className="flex items-center gap-1.5 text-sm font-medium">
              <Monitor className="h-3.5 w-3.5 text-muted-foreground" />
              目标操作系统（在哪个电脑上抓包）
            </label>
            <div className="mt-1.5 grid grid-cols-2 gap-2">
              {platforms.map((p) => (
                <PlatformOption
                  key={`${p.os}/${p.arch}`}
                  p={p}
                  selected={os === p.os && arch === p.arch}
                  onSelect={() => {
                    if (p.available) {
                      setOs(p.os);
                      setArch(p.arch);
                    }
                  }}
                />
              ))}
            </div>
            {selectedPlatform && !selectedPlatform.available && (
              <p className="mt-1 text-xs text-destructive">
                {selectedPlatform.label} 尚未预置，请在服务端运行 make build-agents 生成。
              </p>
            )}
          </div>

          {/* 服务端已部署信息 */}
          <div className="rounded-md border border-border bg-muted/40 px-3 py-2">
            <label className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
              <Server className="h-3.5 w-3.5" />
              当前服务部署信息
            </label>
            {isLoading || !opts?.registry_port ? (
              <p className="mt-1 text-xs text-muted-foreground">正在读取服务端信息…</p>
            ) : (
              <div className="mt-1.5 grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-xs font-mono text-foreground">
                <span className="text-muted-foreground">可达 IP</span>
                <span>{deployHost}</span>
                <span className="text-muted-foreground">registry 端口</span>
                <span>{opts.registry_port}（Agent 回连）</span>
                <span className="text-muted-foreground">ingest 端口</span>
                <span>{opts.ingest_port}（推送抓包数据）</span>
              </div>
            )}
            {opts?.message && <p className="mt-1.5 text-[11px] text-muted-foreground">{opts.message}</p>}
          </div>

          <div>
            <label className="text-sm font-medium">抓包端口（必填）</label>
            <Input
              value={port}
              onChange={(e) => setPort(e.target.value)}
              aria-label="抓包端口"
              inputMode="numeric"
              placeholder="8080"
              className="mt-1.5 font-mono"
            />
            <p className="mt-1 text-xs text-muted-foreground">
              Agent 将抓取该电脑上对该端口的 TCP/UDP 流量（自动生成 BPF 过滤）并推送到服务端会话。
            </p>
          </div>

          <div>
            <label className="text-sm font-medium">解码插件</label>
            <select
              className="mt-1.5 h-9 w-full rounded-md border border-input bg-background px-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
              value={plugin}
              onChange={(e) => setPlugin(e.target.value)}
            >
              <option value="">不指定（仅抓原始包）</option>
              {plugins.map((p) => (
                <option key={p.instance_id} value={p.name}>
                  {p.name}（{p.protocol}）
                </option>
              ))}
            </select>
            <p className="mt-1 text-xs text-muted-foreground">
              {plugins.length === 0
                ? "当前没有已注册的插件，可留空仅抓包；或先启动插件使其注册到 Pipeline。"
                : "留空则只抓包存储原始包；指定插件后服务端会为接收会话绑定该解码插件。"}
            </p>
          </div>

          <div>
            <label className="flex items-center gap-1.5 text-sm font-medium">
              <Radio className="h-3.5 w-3.5 text-muted-foreground" />
              Agent 回连地址（host:port）
            </label>
            <Input
              value={server}
              onChange={(e) => setServer(e.target.value)}
              aria-label="Agent 回连地址"
              placeholder={defaultServer() || "192.168.1.10:9091"}
              spellCheck={false}
              className="mt-1.5 font-mono"
            />
            <p className="mt-1 text-xs text-muted-foreground">
              端口必须是上面的 registry 端口；IP 需为远端 Agent 可达的地址（默认已填入当前服务部署 IP）。
            </p>
          </div>
        </div>
      )}
    </Dialog>
  );
}

function PlatformOption({
  p,
  selected,
  onSelect,
}: {
  p: AgentPlatform;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      disabled={!p.available}
      aria-pressed={selected}
      className={`flex items-center gap-2 rounded-md border px-2.5 py-2 text-sm transition-colors ${
        selected
          ? "border-primary/60 bg-primary/10 text-foreground"
          : "border-border bg-background text-muted-foreground"
      } ${p.available ? "cursor-pointer hover:bg-muted/60" : "cursor-not-allowed opacity-50"}`}
    >
      {selected ? (
        <Check className="h-4 w-4 text-primary" />
      ) : (
        <span className="h-4 w-4 rounded-full border border-border" />
      )}
      <span className="truncate">{p.label}</span>
      {p.exe ? <span className="ml-auto font-mono text-[10px] text-muted-foreground">.exe</span> : null}
    </button>
  );
}

function AwaitingAgentPanel({
  attached,
  packets,
  events,
}: {
  attached: boolean;
  packets: number;
  events: number;
}) {
  const steps = [
    {
      label: "Agent 已生成并下载",
      detail: "已在服务端创建接收会话，zip 已保存到浏览器下载目录",
      done: true,
    },
    {
      label: "解压并双击运行 Agent",
      detail: "在目标电脑解压 zip，双击运行 gta-agent（.exe 视平台而定）",
      done: true,
    },
    {
      label: attached ? "Agent 已连接 · 抓包中" : "等待 Agent 连接…",
      detail: attached
        ? `已收到 ${packets.toLocaleString()} packets · 已解析 ${events.toLocaleString()} events`
        : "Agent 正在回连服务端，连接建立后会自动开始抓包（无需任何参数）",
      done: attached,
    },
  ];
  return (
    <div className="space-y-3">
      <ol className="space-y-2.5">
        {steps.map((s, i) => (
          <li key={i} className="flex gap-2.5">
            <span
              className={`mt-0.5 flex h-5 w-5 shrink-0 items-center justify-center rounded-full ${
                s.done ? "bg-primary/15 text-primary" : "bg-muted text-muted-foreground"
              }`}
            >
              {s.done ? <Check className="h-3.5 w-3.5" /> : <Loader2 className="h-3.5 w-3.5 animate-spin" />}
            </span>
            <div>
              <p className={`text-sm ${s.done ? "font-medium text-foreground" : "text-muted-foreground"}`}>
                {s.label}
              </p>
              <p className="text-xs text-muted-foreground">{s.detail}</p>
            </div>
          </li>
        ))}
      </ol>

      {attached ? (
        <div className="grid grid-cols-2 gap-2 rounded-md border border-border bg-muted/40 p-3">
          <div>
            <p className="text-xs text-muted-foreground">Packets</p>
            <p className="text-lg font-semibold tabular-nums">{packets.toLocaleString()}</p>
          </div>
          <div>
            <p className="text-xs text-muted-foreground">Events</p>
            <p className="text-lg font-semibold tabular-nums">{events.toLocaleString()}</p>
          </div>
        </div>
      ) : (
        <p className="rounded-md border border-border bg-muted/30 px-3 py-2 text-xs text-muted-foreground">
          正在等待 Agent 回连… 若长时间无变化，请确认目标电脑能访问上面的回连地址，且压缩包中的
          config.embedded.json 与 Agent 在同一目录。
        </p>
      )}
    </div>
  );
}