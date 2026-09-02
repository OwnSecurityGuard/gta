import { useEffect, useRef, useState } from "react";
import QRCode from "react-qr-code";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { useProxyServerConfig, useUpdateProxyServerConfig, useListPlugins } from "@/hooks/use-mcp";
import type { ProxyConfigState } from "@/types/proxy";
import { toast } from "@/components/ui/toast";
import {
  ArrowRight,
  Cable,
  Check,
  ChevronDown,
  Copy,
  CopyCheck,
  MonitorSmartphone,
  RefreshCw,
  Server,
  Smartphone,
  Wifi,
  X,
} from "lucide-react";

interface ProxyConfigDialogProps {
  open: boolean;
  onClose: () => void;
  /** 跳转到指定抓包会话查看代理抓包数据；不传则隐藏"查看会话数据"入口。 */
  onNavigateToSession?: (sessionId: string) => void;
}

/** 字节数人类可读格式化（B/KB/MB）。 */
function formatBytes(n: number): string {
  if (!n || n <= 0) return "0 B";
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(2)} MB`;
}

/** last_data_unix（毫秒）→ 相对时间文案。 */
function lastDataText(unixMs: number): string {
  if (!unixMs) return "从未";
  const sec = Math.max(0, Math.floor((Date.now() - unixMs) / 1000));
  if (sec < 5) return "刚刚";
  if (sec < 60) return `${sec} 秒前`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min} 分钟前`;
  return `${Math.floor(min / 60)} 小时前`;
}

/** host:port 简单校验，允许空值（提交时后端再校验）。 */
function hostPortError(value: string): string | null {
  if (!value) return null;
  const trimmed = value.trim();
  if (!trimmed.includes(":")) return "格式应为 host:port";
  const port = trimmed.split(":").pop();
  if (!port || !/^\d+$/.test(port)) return "端口应为数字";
  const n = Number(port);
  if (n < 1 || n > 65535) return "端口范围 1-65535";
  return null;
}

interface CollapseProps {
  title: string;
  subtitle?: string;
  defaultOpen?: boolean;
  children: React.ReactNode;
}

/** 折叠配置区：连接配置默认展开，筛选等高级项默认收起，减少主流程干扰。 */
function Collapse({ title, subtitle, defaultOpen = false, children }: CollapseProps) {
  const [openState, setOpenState] = useState(defaultOpen);
  return (
    <div className="rounded-lg border border-border">
      <button
        type="button"
        onClick={() => setOpenState((o) => !o)}
        aria-expanded={openState}
        className="flex w-full items-center gap-2 rounded-lg px-3 py-2.5 text-left transition-colors hover:bg-muted/50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40"
      >
        <ChevronDown
          className={`h-4 w-4 shrink-0 text-muted-foreground transition-transform ${openState ? "rotate-0" : "-rotate-90"}`}
        />
        <span className="min-w-0">
          <span className="block truncate text-sm font-medium">{title}</span>
          {subtitle && (
            <span className="block truncate text-xs font-normal text-muted-foreground">{subtitle}</span>
          )}
        </span>
      </button>
      {openState && <div className="space-y-3 border-t border-border px-3 py-3">{children}</div>}
    </div>
  );
}

interface StepItem {
  key: string;
  label: string;
  desc: string;
  done: boolean;
}

/** 四步连接状态步骤条：代理服务 → 抓包会话 → 手机连接 → 数据流入。 */
function ConnectionSteps({ steps }: { steps: StepItem[] }) {
  return (
    <div className="flex items-center">
      {steps.map((s, i) => (
        <div key={s.key} className={`flex items-center ${i > 0 ? "flex-1" : ""}`}>
          {i > 0 && (
            <div
              className={`mx-1 h-0.5 flex-1 rounded ${s.done ? "bg-emerald-500/60" : "bg-border"}`}
              aria-hidden
            />
          )}
          <div className="flex min-w-0 flex-col items-center gap-0.5">
            <div
              className={`flex h-6 w-6 shrink-0 items-center justify-center rounded-full text-xs font-semibold ${
                s.done ? "bg-emerald-500 text-white" : "border-2 border-border bg-background text-muted-foreground"
              }`}
              title={`${s.label}：${s.desc}`}
            >
              {s.done ? <Check className="h-3.5 w-3.5" /> : i + 1}
            </div>
            <span
              className={`max-w-[64px] truncate text-center text-[11px] leading-tight ${
                s.done ? "text-foreground" : "text-muted-foreground"
              }`}
            >
              {s.label}
            </span>
          </div>
        </div>
      ))}
    </div>
  );
}

interface QuickChipProps {
  label: string;
  onClick: () => void;
  disabled?: boolean;
}

function QuickChip({ label, onClick, disabled }: QuickChipProps) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className="inline-flex items-center rounded-md border border-border bg-background px-2 py-0.5 text-xs font-medium text-foreground transition-colors hover:border-primary/40 hover:bg-muted/50 disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40"
    >
      {label}
    </button>
  );
}

/** 代理服务器配置对话框：状态步骤条 + 扫码即连 + 折叠配置。
 * 帧边界判定是协议语义，由绑定到会话的解码插件按连接自行处理。 */
export function ProxyConfigDialog({ open, onClose, onNavigateToSession }: ProxyConfigDialogProps) {
  const { data, isLoading, refetch } = useProxyServerConfig();
  const update = useUpdateProxyServerConfig();
  const plugins = useListPlugins();
  const state = data?.state;

  // 表单本地状态；服务端配置仅在"未编辑过"时同步，避免 5s 轮询覆盖用户正在输入的内容。
  const [listenAddr, setListenAddr] = useState("");
  const [serverAddr, setServerAddr] = useState("");
  const [plugin, setPlugin] = useState("");
  const [filterHosts, setFilterHosts] = useState("");
  const [filterPorts, setFilterPorts] = useState("");
  const [saved, setSaved] = useState(false);
  const dirtyRef = useRef(false);
  const syncedKeyRef = useRef("");

  const [copied, setCopied] = useState<"uri" | "addr" | null>(null);

  // 打开弹窗时重置"已编辑"标记，并从最新配置初始化表单。
  useEffect(() => {
    if (!open) return;
    dirtyRef.current = false;
    syncedKeyRef.current = "";
    setCopied(null);
  }, [open]);

  // 服务端配置到达/变化时，若用户未编辑则同步到表单（首次加载、保存成功后均走这里）。
  useEffect(() => {
    if (!state || dirtyRef.current) return;
    const key = JSON.stringify([
      state.listen_addr,
      state.server_addr,
      state.plugin,
      state.include_hosts,
      state.include_ports,
    ]);
    if (key === syncedKeyRef.current) return;
    syncedKeyRef.current = key;
    setListenAddr(state.listen_addr);
    setServerAddr(state.server_addr);
    setPlugin(state.plugin ?? "");
    setFilterHosts((state.include_hosts ?? []).join(","));
    setFilterPorts((state.include_ports ?? []).join(","));
  }, [state]);

  const connectAddr = state?.connect_addr ?? "";
  const singboxUri = state?.singbox_uri ?? "";
  const agentUp = !!state?.agent_running;
  const sessionUp = !!state?.session_running;
  const activeConns = state?.active_conns ?? 0;
  const totalConns = state?.total_conns ?? 0;
  const totalBytes = state?.total_bytes ?? 0;
  const pluginOptions = plugins.data?.plugins ?? [];
  const lanIp = state?.lan_ip ?? "";

  const listenError = hostPortError(listenAddr);
  const serverError = hostPortError(serverAddr);

  function syncToState(st: ProxyConfigState) {
    syncedKeyRef.current = JSON.stringify([
      st.listen_addr,
      st.server_addr,
      st.plugin,
      st.include_hosts,
      st.include_ports,
    ]);
    setListenAddr(st.listen_addr);
    setServerAddr(st.server_addr);
    setPlugin(st.plugin ?? "");
    setFilterHosts((st.include_hosts ?? []).join(","));
    setFilterPorts((st.include_ports ?? []).join(","));
  }

  function handleSave() {
    if (listenError || serverError) {
      toast.error("请修正地址格式后再保存");
      return;
    }
    update.mutate(
      {
        listenAddr: listenAddr.trim(),
        serverAddr: serverAddr.trim(),
        plugin: plugin.trim(),
        // 筛选以逗号分隔输入；空文本传空数组=清空筛选（抓全部连接）。
        includeHosts: filterHosts
          .split(",")
          .map((s) => s.trim())
          .filter(Boolean),
        includePorts: filterPorts
          .split(",")
          .map((s) => Number(s.trim()))
          .filter((n) => Number.isInteger(n) && n >= 1 && n <= 65535),
      },
      {
        onSuccess: (res) => {
          // 以应用后的状态同步表单 + 重置编辑标记，二维码/状态徽章随之更新。
          const st = res?.state;
          if (st) syncToState(st);
          dirtyRef.current = false;
          setSaved(true);
          toast.success("代理服务器配置已应用", res?.message);
          setTimeout(() => setSaved(false), 1200);
        },
        onError: (err) => {
          toast.error("应用失败", err.message);
        },
      },
    );
  }

  async function handleCopy(text: string, kind: "uri" | "addr") {
    if (!text) return;
    try {
      await navigator.clipboard.writeText(text);
      setCopied(kind);
      toast.success(kind === "uri" ? "已复制 sing-box 导入链接" : "已复制连接地址", text);
      setTimeout(() => setCopied((c) => (c === kind ? null : c)), 1500);
    } catch {
      toast.info("复制失败，请手动选择地址", text);
    }
  }

  function applyQuickListen(template: string) {
    const port = listenAddr.includes(":") ? listenAddr.split(":").pop() : "12000";
    if (template === "lan" && lanIp) {
      setListenAddr(`${lanIp}:${port}`);
    } else {
      setListenAddr(`0.0.0.0:${port}`);
    }
    dirtyRef.current = true;
  }

  function applyQuickServer(all = false) {
    const port = serverAddr.includes(":") ? serverAddr.split(":").pop() : "9090";
    setServerAddr(`${all ? "0.0.0.0" : "127.0.0.1"}:${port}`);
    dirtyRef.current = true;
  }

  // 四步连接状态（数据流入以累计字节>0 判定，最近数据时间做辅助提示）。
  const steps: StepItem[] = [
    {
      key: "agent",
      label: "代理服务",
      done: agentUp,
      desc: agentUp ? `Agent 运行中 · PID ${state?.agent_pid ?? "?"}` : "Agent 未运行",
    },
    {
      key: "session",
      label: "抓包会话",
      done: sessionUp,
      desc: sessionUp ? "常驻会话运行中" : "会话未运行",
    },
    {
      key: "phone",
      label: "手机连接",
      done: activeConns > 0,
      desc: activeConns > 0 ? `${activeConns} 路连接活跃` : "等待手机接入",
    },
    {
      key: "data",
      label: "数据流入",
      done: totalBytes > 0,
      desc: totalBytes > 0 ? `已收 ${formatBytes(totalBytes)}` : "暂无数据",
    },
  ];

  const phoneConnected = activeConns > 0;
  const qrValue = singboxUri || connectAddr;
  const qrLabel = singboxUri ? "sing-box 扫码导入" : "手机代理扫码连接";

  return (
    <Dialog
      open={open}
      onClose={onClose}
      icon={<Cable className="h-5 w-5" />}
      title="代理服务器配置"
      description="常驻服务，手机扫码即连；绿色进度表示链路已打通。"
      className="max-w-3xl"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            <X className="h-4 w-4" />
            关闭
          </Button>
          <Button onClick={handleSave} disabled={update.isPending || isLoading || !!listenError || !!serverError}>
            {saved ? (
              <>
                <Check className="h-4 w-4" />
                已应用
              </>
            ) : update.isPending ? (
              "应用中…"
            ) : (
              "保存并应用"
            )}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        {/* 状态卡：步骤条 + 实时活动 + 跳转会话 */}
        <section className="rounded-xl border border-border bg-muted/30 p-4">
          <div className="flex items-center justify-between gap-2">
            <div className="flex items-center gap-2">
              <Wifi className="h-4 w-4 text-muted-foreground" />
              <h4 className="text-sm font-semibold">连接状态</h4>
            </div>
            <div className="flex items-center gap-2">
              {sessionUp && state?.session_id && onNavigateToSession && (
                <Button
                  variant="outline"
                  size="sm"
                  className="h-7 gap-1 px-2 text-xs"
                  onClick={() => onNavigateToSession(state.session_id)}
                  title={`跳转到会话 ${state.session_id}`}
                >
                  查看会话数据
                  <ArrowRight className="h-3.5 w-3.5" />
                </Button>
              )}
              <Button
                variant="ghost"
                size="icon"
                className="h-7 w-7"
                onClick={() => void refetch()}
                title="刷新状态"
                aria-label="刷新状态"
              >
                <RefreshCw className="h-3.5 w-3.5" />
              </Button>
            </div>
          </div>

          <div className="mt-3">
            <ConnectionSteps steps={steps} />
          </div>

          {/* 实时活动明细 */}
          <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-border pt-2.5 text-xs text-muted-foreground">
            <span className={phoneConnected ? "font-medium text-emerald-600" : ""}>
              活跃连接 <span className="font-mono">{activeConns}</span>
            </span>
            <span>
              累计连接 <span className="font-mono">{totalConns}</span>
            </span>
            <span>
              累计数据 <span className="font-mono">{formatBytes(totalBytes)}</span>
            </span>
            <span>
              最近数据 <span className="font-mono">{lastDataText(state?.last_data_unix ?? 0)}</span>
            </span>
            {!phoneConnected && (
              <span className="w-full pt-0.5">
                手机尚未接入：请用右侧二维码完成配置，接入后此卡片实时更新。
              </span>
            )}
          </div>
        </section>

        {/* 移动端：QR 置顶；桌面端：左右分栏 */}
        <div className="grid gap-4 lg:grid-cols-[1fr_280px]">
          {/* 左：配置表单（桌面端 order-1） */}
          <div className="order-2 space-y-2 lg:order-1">
            <Collapse title="连接配置" subtitle="手机连哪里、数据推到哪里" defaultOpen>
              <div className="space-y-3">
                <div>
                  <label htmlFor="proxy-listen-addr" className="text-sm font-medium">
                    代理监听地址（手机连接）
                  </label>
                  <Input
                    id="proxy-listen-addr"
                    value={listenAddr}
                    onChange={(e) => {
                      dirtyRef.current = true;
                      setListenAddr(e.target.value);
                    }}
                    aria-label="代理监听地址"
                    placeholder="0.0.0.0:12000"
                    className={`mt-1.5 font-mono ${listenError ? "border-destructive focus-visible:ring-destructive/30" : ""}`}
                  />
                  {listenError ? (
                    <p className="mt-1 text-xs text-destructive">{listenError}</p>
                  ) : (
                    <p className="mt-1 text-xs text-muted-foreground">
                      gta-singbox-agent 的 HTTP CONNECT 代理监听地址。手机经局域网访问时必须绑定{" "}
                      <code className="font-mono">0.0.0.0</code> 或具体局域网 IP。
                    </p>
                  )}
                  <div className="mt-1.5 flex flex-wrap items-center gap-1.5">
                    <QuickChip
                      label="0.0.0.0:12000"
                      onClick={() => {
                        setListenAddr("0.0.0.0:12000");
                        dirtyRef.current = true;
                      }}
                    />
                    <QuickChip
                      label={`本机IP:${listenAddr.includes(":") ? listenAddr.split(":").pop() : "12000"}`}
                      onClick={() => applyQuickListen("lan")}
                      disabled={!lanIp}
                    />
                  </div>
                </div>

                <div>
                  <label htmlFor="proxy-server-addr" className="text-sm font-medium">
                    数据服务地址（agent 推送）
                  </label>
                  <Input
                    id="proxy-server-addr"
                    value={serverAddr}
                    onChange={(e) => {
                      dirtyRef.current = true;
                      setServerAddr(e.target.value);
                    }}
                    aria-label="数据服务地址"
                    placeholder="127.0.0.1:9090"
                    className={`mt-1.5 font-mono ${serverError ? "border-destructive focus-visible:ring-destructive/30" : ""}`}
                  />
                  {serverError ? (
                    <p className="mt-1 text-xs text-destructive">{serverError}</p>
                  ) : (
                    <p className="mt-1 text-xs text-muted-foreground">
                      mobile Source（常驻抓包会话）的 gRPC 监听地址，agent 推送连接级数据到这里。
                    </p>
                  )}
                  <div className="mt-1.5 flex flex-wrap items-center gap-1.5">
                    <QuickChip
                      label="127.0.0.1:9090"
                      onClick={() => applyQuickServer(false)}
                    />
                    <QuickChip
                      label="0.0.0.0:9090"
                      onClick={() => applyQuickServer(true)}
                    />
                  </div>
                </div>
              </div>
            </Collapse>

            <Collapse title="解码插件" subtitle="绑定插件解码流量；分帧/重组由插件自身实现">
              <div>
                <label htmlFor="proxy-plugin" className="text-sm font-medium">
                  解码插件
                </label>
                <select
                  id="proxy-plugin"
                  className="mt-1.5 h-9 w-full rounded-md border border-input bg-background px-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
                  value={plugin}
                  onChange={(e) => {
                    dirtyRef.current = true;
                    setPlugin(e.target.value);
                  }}
                >
                  <option value="">仅抓原始包（不解码）</option>
                  {pluginOptions.map((p) => (
                    <option key={p.name} value={p.name}>
                      {p.name}
                    </option>
                  ))}
                </select>
                <p className="mt-1 text-xs text-muted-foreground">
                  代理抓包会话绑定该插件解码原始流量，生成协议事件与连接详情。需插件已注册，空为仅抓原始包。
                  平台按数据块透传字节流，协议分帧（粘包/半包与帧边界）由插件自行处理。
                </p>
              </div>
            </Collapse>

            <Collapse title="连接筛选" subtitle="只抓指定主机/端口的连接，减少无关流量">
              <div className="grid gap-3 sm:grid-cols-2">
                <div>
                  <label htmlFor="proxy-filter-hosts" className="text-sm font-medium">
                    筛选目标主机（逗号分隔）
                  </label>
                  <Input
                    id="proxy-filter-hosts"
                    value={filterHosts}
                    onChange={(e) => {
                      dirtyRef.current = true;
                      setFilterHosts(e.target.value);
                    }}
                    aria-label="筛选目标主机"
                    placeholder="1.2.3.4, api.game.com"
                    className="mt-1.5 font-mono"
                  />
                  <p className="mt-1 text-xs text-muted-foreground">
                    仅抓取目标主机（CONNECT 中的 host）在此列表内的连接；IP 或域名，不区分大小写。留空=不筛选。
                  </p>
                </div>
                <div>
                  <label htmlFor="proxy-filter-ports" className="text-sm font-medium">
                    筛选目标端口（逗号分隔）
                  </label>
                  <Input
                    id="proxy-filter-ports"
                    value={filterPorts}
                    onChange={(e) => {
                      dirtyRef.current = true;
                      setFilterPorts(e.target.value);
                    }}
                    aria-label="筛选目标端口"
                    placeholder="443, 8443"
                    className="mt-1.5 font-mono"
                  />
                  <p className="mt-1 text-xs text-muted-foreground">
                    仅抓取目标端口在此列表内的连接；与主机同时设置时须同时命中。留空=不筛选。
                  </p>
                </div>
              </div>
            </Collapse>

            {update.isError && (
              <p className="text-xs text-destructive">应用失败：{update.error?.message}</p>
            )}
          </div>

          {/* 右：二维码 + 连接信息（移动端 order-1） */}
          <div className="order-1 lg:order-2">
            <div className="flex flex-col gap-3 rounded-xl border border-border bg-muted/40 p-4">
              <div className="flex items-center gap-2 text-sm font-medium">
                <Smartphone className="h-4 w-4 text-muted-foreground" />
                <span>手机连接</span>
              </div>

              <div className="flex justify-center rounded-lg bg-white p-3 shadow-sm">
                {isLoading ? (
                  <div className="flex h-44 w-44 items-center justify-center text-xs text-muted-foreground">
                    加载中…
                  </div>
                ) : qrValue ? (
                  <QRCode value={qrValue} size={176} bgColor="#ffffff" fgColor="#0f172a" />
                ) : (
                  <div className="flex h-44 w-44 items-center justify-center text-center text-xs text-muted-foreground">
                    暂无局域网地址，无法生成二维码
                  </div>
                )}
              </div>

              <div className="space-y-2">
                <p className="text-center text-xs text-muted-foreground">{qrLabel}</p>

                <button
                  type="button"
                  onClick={() => qrValue && handleCopy(qrValue, singboxUri ? "uri" : "addr")}
                  disabled={!qrValue}
                  className="flex w-full items-center gap-2 rounded-md border border-border bg-background px-2.5 py-1.5 text-left text-sm text-foreground transition-colors hover:border-primary/40 disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40"
                  title={singboxUri ? "复制 sing-box 导入链接" : "复制连接地址"}
                >
                  {copied === (singboxUri ? "uri" : "addr") ? (
                    <CopyCheck className="h-3.5 w-3.5 shrink-0 text-emerald-500" />
                  ) : (
                    <Copy className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                  )}
                  <span className="min-w-0 flex-1 truncate font-mono text-xs">{qrValue || "——"}</span>
                </button>

                {singboxUri ? (
                  <div className="rounded-md border border-emerald-500/20 bg-emerald-500/5 px-2.5 py-2 text-xs leading-relaxed text-emerald-700">
                    <div className="mb-1 flex items-center gap-1 font-medium">
                      <Check className="h-3 w-3" />
                      sing-box（SFA）一键导入
                    </div>
                    用 sing-box「添加配置 → 扫描二维码」导入，自动生成 TUN 配置并走 HTTP 代理连接到{" "}
                    <code className="font-mono">{connectAddr || "本机"}</code>，无需手动填写。
                  </div>
                ) : (
                  <div className="rounded-md bg-muted/60 px-2.5 py-2 text-xs leading-relaxed text-muted-foreground">
                    手机代理软件（如 sing-box / Clash）添加 HTTP 代理，服务器填{" "}
                    <code className="font-mono">{state?.lan_ip || "本机IP"}</code>，端口填代理端口。
                  </div>
                )}
              </div>

              <div className="border-t border-border pt-3">
                <div className="flex items-center gap-2 text-xs text-muted-foreground">
                  <Server className="h-3.5 w-3.5" />
                  <span className="truncate">
                    数据推送到 <span className="font-mono">{serverAddr || "127.0.0.1:9090"}</span>
                  </span>
                </div>
                <div className="mt-1 flex items-center gap-2 text-xs text-muted-foreground">
                  <MonitorSmartphone className="h-3.5 w-3.5" />
                  <span className="truncate">
                    手机代理填 <span className="font-mono">{connectAddr || listenAddr || "IP:端口"}</span>
                  </span>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </Dialog>
  );
}
