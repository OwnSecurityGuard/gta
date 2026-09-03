import { useEffect, useState } from "react";
import QRCode from "react-qr-code";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { useProxyLeases, useCreateProxyLease, useReleaseProxyLease, useListPlugins } from "@/hooks/use-mcp";
import type { ProxyLease } from "@/types/proxy";
import { toast } from "@/components/ui/toast";
import {
  ArrowRight,
  Cable,
  Check,
  ChevronDown,
  Copy,
  CopyCheck,
  MonitorSmartphone,
  Plus,
  Smartphone,
  Trash2,
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

interface CollapseProps {
  title: string;
  subtitle?: string;
  defaultOpen?: boolean;
  children: React.ReactNode;
}

/** 折叠配置区：筛选等高级项默认收起，减少主流程干扰。 */
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

/** 代理抓包租约对话框：按用户/设备创建独立会话，多用户互不串流、互不抢配置。
 * 每个租约 = 独立 mobile 会话 + 独立 gta-singbox-agent + 私有筛选配置。 */
export function ProxyConfigDialog({ open, onClose, onNavigateToSession }: ProxyConfigDialogProps) {
  const leasesQuery = useProxyLeases();
  const createLease = useCreateProxyLease();
  const releaseLease = useReleaseProxyLease();
  const plugins = useListPlugins();
  const leases = leasesQuery.data?.leases ?? [];

  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [device, setDevice] = useState("");
  const [plugin, setPlugin] = useState("");
  const [filterHosts, setFilterHosts] = useState("");
  const [filterPorts, setFilterPorts] = useState("");
  const [copied, setCopied] = useState<"uri" | "addr" | null>(null);

  // 选中租约：优先保持用户选择，失效时回退到第一个。
  const selected = leases.find((l) => l.lease_id === selectedId) ?? leases[0] ?? null;
  const pluginOptions = plugins.data?.plugins ?? [];

  // 打开时重置创建表单与复制状态。
  useEffect(() => {
    if (!open) return;
    setCreating(false);
    setCopied(null);
  }, [open]);

  // 无租约时自动进入创建视图。
  useEffect(() => {
    if (!open) return;
    if (leases.length === 0 && !leasesQuery.isLoading) setCreating(true);
  }, [open, leases.length, leasesQuery.isLoading]);

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

  function handleCreate() {
    createLease.mutate(
      {
        plugin: plugin.trim() || undefined,
        device: device.trim() || undefined,
        includeHosts: filterHosts.split(",").map((s) => s.trim()).filter(Boolean),
        includePorts: filterPorts
          .split(",")
          .map((s) => Number(s.trim()))
          .filter((n) => Number.isInteger(n) && n >= 1 && n <= 65535),
      },
      {
        onSuccess: (res) => {
          const lease = res?.lease;
          if (lease?.lease_id) setSelectedId(lease.lease_id);
          setCreating(false);
          setDevice("");
          setPlugin("");
          setFilterHosts("");
          setFilterPorts("");
          toast.success("代理租约已创建", lease?.connect_addr ?? "");
        },
        onError: (err) => {
          toast.error("创建失败", err.message);
        },
      },
    );
  }

  function handleRelease(leaseId: string) {
    releaseLease.mutate(leaseId, {
      onSuccess: (res) => {
        if (selectedId === leaseId) setSelectedId(null);
        toast.success("租约已释放", res?.message);
      },
      onError: (err) => {
        toast.error("释放失败", err.message);
      },
    });
  }

  // ===== 创建租约表单 =====
  if (creating) {
    return (
      <Dialog
        open={open}
        onClose={onClose}
        icon={<Cable className="h-5 w-5" />}
        title="新建代理租约"
        description="为当前用户/设备创建独立代理抓包会话，互不串流、互不抢配置。"
        className="max-w-xl"
        footer={
          <>
            <Button
              variant="outline"
              onClick={() => {
                // 已有租约时允许返回列表，否则关闭弹窗。
                if (leases.length > 0) setCreating(false);
                else onClose();
              }}
            >
              <X className="h-4 w-4" />
              取消
            </Button>
            <Button onClick={handleCreate} disabled={createLease.isPending}>
              {createLease.isPending ? "创建中…" : (
                <>
                  <Plus className="h-4 w-4" />
                  创建租约
                </>
              )}
            </Button>
          </>
        }
      >
        <div className="space-y-3">
          <div>
            <label htmlFor="lease-device" className="text-sm font-medium">
              设备标签
            </label>
            <Input
              id="lease-device"
              value={device}
              onChange={(e) => setDevice(e.target.value)}
              aria-label="设备标签"
              placeholder="如 alice-phone（可选，便于识别）"
              className="mt-1.5"
            />
          </div>

          <div>
            <label htmlFor="lease-plugin" className="text-sm font-medium">
              解码插件
            </label>
            <select
              id="lease-plugin"
              className="mt-1.5 h-9 w-full rounded-md border border-input bg-background px-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
              value={plugin}
              onChange={(e) => setPlugin(e.target.value)}
            >
              <option value="">仅抓原始包（不解码）</option>
              {pluginOptions.map((p) => (
                <option key={p.name} value={p.name}>
                  {p.name}
                </option>
              ))}
            </select>
            <p className="mt-1 text-xs text-muted-foreground">
              租约会话绑定该插件解码流量；分帧/重组由插件自身实现，空为仅抓原始包。
            </p>
          </div>

          <Collapse title="连接筛选" subtitle="只抓指定主机/端口的连接，减少无关流量">
            <div className="grid gap-3 sm:grid-cols-2">
              <div>
                <label htmlFor="lease-filter-hosts" className="text-sm font-medium">
                  筛选目标主机（逗号分隔）
                </label>
                <Input
                  id="lease-filter-hosts"
                  value={filterHosts}
                  onChange={(e) => setFilterHosts(e.target.value)}
                  aria-label="筛选目标主机"
                  placeholder="1.2.3.4, api.game.com"
                  className="mt-1.5 font-mono"
                />
                <p className="mt-1 text-xs text-muted-foreground">
                  仅抓取目标主机在此列表内的连接；留空=不筛选。
                </p>
              </div>
              <div>
                <label htmlFor="lease-filter-ports" className="text-sm font-medium">
                  筛选目标端口（逗号分隔）
                </label>
                <Input
                  id="lease-filter-ports"
                  value={filterPorts}
                  onChange={(e) => setFilterPorts(e.target.value)}
                  aria-label="筛选目标端口"
                  placeholder="443, 8443"
                  className="mt-1.5 font-mono"
                />
                <p className="mt-1 text-xs text-muted-foreground">
                  仅抓取目标端口在此列表内的连接；留空=不筛选。
                </p>
              </div>
            </div>
          </Collapse>

          {createLease.isError && (
            <p className="text-xs text-destructive">创建失败：{createLease.error?.message}</p>
          )}
        </div>
      </Dialog>
    );
  }

  // ===== 租约列表 + 选中详情 =====
  return (
    <Dialog
      open={open}
      onClose={onClose}
      icon={<Cable className="h-5 w-5" />}
      title="移动代理租约"
      description="每个租约是独立抓包会话，多用户互不串流、互不抢配置。"
      className="max-w-3xl"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            <X className="h-4 w-4" />
            关闭
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        {/* 租约列表 */}
        <section className="space-y-2">
          <div className="flex items-center justify-between">
            <h4 className="text-sm font-semibold">我的租约（{leases.length}）</h4>
            <Button variant="outline" size="sm" className="h-7 gap-1 px-2 text-xs" onClick={() => setCreating(true)}>
              <Plus className="h-3.5 w-3.5" />
              新建租约
            </Button>
          </div>

          {leasesQuery.isLoading && leases.length === 0 ? (
            <p className="py-6 text-center text-xs text-muted-foreground">加载中…</p>
          ) : leases.length === 0 ? (
            <p className="py-6 text-center text-xs text-muted-foreground">暂无租约，点击「新建租约」开始。</p>
          ) : (
            <div className="flex flex-wrap gap-2">
              {leases.map((l) => {
                const active = l.lease_id === selected?.lease_id;
                return (
                  <button
                    key={l.lease_id}
                    type="button"
                    onClick={() => setSelectedId(l.lease_id)}
                    className={`flex items-center gap-2 rounded-md border px-2.5 py-1.5 text-xs transition-colors ${
                      active
                        ? "border-primary bg-primary/10 text-foreground"
                        : "border-border bg-background text-muted-foreground hover:border-primary/40 hover:bg-muted/50"
                    }`}
                  >
                    <span className={`h-1.5 w-1.5 rounded-full ${l.session_running ? "bg-emerald-500" : "bg-muted-foreground"}`} />
                    <span className="max-w-[160px] truncate">{l.device || l.lease_id}</span>
                  </button>
                );
              })}
            </div>
          )}
        </section>

        {selected ? <LeaseDetail lease={selected} onNavigateToSession={onNavigateToSession} onRelease={handleRelease} onCopy={handleCopy} copied={copied} /> : null}
      </div>
    </Dialog>
  );
}

interface LeaseDetailProps {
  lease: ProxyLease;
  onNavigateToSession?: (sessionId: string) => void;
  onRelease: (leaseId: string) => void;
  onCopy: (text: string, kind: "uri" | "addr") => void;
  copied: "uri" | "addr" | null;
}

function LeaseDetail({ lease, onNavigateToSession, onRelease, onCopy, copied }: LeaseDetailProps) {
  const agentUp = !!lease.agent_running;
  const sessionUp = !!lease.session_running;
  const activeConns = lease.active_conns ?? 0;
  const totalConns = lease.total_conns ?? 0;
  const totalBytes = lease.total_bytes ?? 0;
  const connectAddr = lease.connect_addr ?? "";
  const singboxUri = lease.singbox_uri ?? "";
  const phoneConnected = activeConns > 0;
  const qrValue = singboxUri || connectAddr;
  const qrLabel = singboxUri ? "sing-box 扫码导入" : "手机代理扫码连接";

  const steps: StepItem[] = [
    {
      key: "agent",
      label: "代理服务",
      done: agentUp,
      desc: agentUp ? `Agent 运行中 · PID ${lease.agent_pid ?? "?"}` : "Agent 未运行",
    },
    {
      key: "session",
      label: "抓包会话",
      done: sessionUp,
      desc: sessionUp ? "独立会话运行中" : "会话未运行",
    },
    {
      key: "phone",
      label: "手机连接",
      done: phoneConnected,
      desc: phoneConnected ? `${activeConns} 路连接活跃` : "等待手机接入",
    },
    {
      key: "data",
      label: "数据流入",
      done: totalBytes > 0,
      desc: totalBytes > 0 ? `已收 ${formatBytes(totalBytes)}` : "暂无数据",
    },
  ];

  return (
    <>
      {/* 状态卡：步骤条 + 实时活动 + 跳转会话 */}
      <section className="rounded-xl border border-border bg-muted/30 p-4">
        <div className="flex items-center justify-between gap-2">
          <div className="flex items-center gap-2">
            <Wifi className="h-4 w-4 text-muted-foreground" />
            <h4 className="text-sm font-semibold">{lease.device || "连接状态"}</h4>
          </div>
          <div className="flex items-center gap-2">
            {sessionUp && lease.session_id && onNavigateToSession && (
              <Button
                variant="outline"
                size="sm"
                className="h-7 gap-1 px-2 text-xs"
                onClick={() => onNavigateToSession(lease.session_id)}
                title={`跳转到会话 ${lease.session_id}`}
              >
                查看会话数据
                <ArrowRight className="h-3.5 w-3.5" />
              </Button>
            )}
            <Button
              variant="outline"
              size="sm"
              className="h-7 gap-1 px-2 text-xs text-destructive hover:bg-destructive/10 hover:text-destructive"
              onClick={() => onRelease(lease.lease_id)}
              title="释放租约（停止会话并回收端口）"
            >
              <Trash2 className="h-3.5 w-3.5" />
              释放
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
            最近数据 <span className="font-mono">{lastDataText(lease.last_data_unix ?? 0)}</span>
          </span>
          {!phoneConnected && (
            <span className="w-full pt-0.5">
              手机尚未接入：请用右侧二维码完成配置，接入后此卡片实时更新。
            </span>
          )}
        </div>
      </section>

      {/* 二维码 + 连接信息 */}
      <section className="rounded-xl border border-border bg-muted/40 p-4">
        <div className="flex flex-col items-center gap-3">
          <div className="flex w-full items-center justify-between gap-2 text-sm font-medium">
            <span className="flex items-center gap-2">
              <Smartphone className="h-4 w-4 text-muted-foreground" />
              手机连接
            </span>
            <span className="truncate font-mono text-xs text-muted-foreground">
              {lease.listen_addr || ""}
            </span>
          </div>

          <div className="flex justify-center rounded-lg bg-white p-3 shadow-sm">
            {qrValue ? (
              <QRCode value={qrValue} size={176} bgColor="#ffffff" fgColor="#0f172a" />
            ) : (
              <div className="flex h-44 w-44 items-center justify-center text-center text-xs text-muted-foreground">
                暂无局域网地址，无法生成二维码
              </div>
            )}
          </div>

          <p className="text-center text-xs text-muted-foreground">{qrLabel}</p>

          <button
            type="button"
            onClick={() => qrValue && onCopy(qrValue, singboxUri ? "uri" : "addr")}
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
            <div className="w-full rounded-md border border-emerald-500/20 bg-emerald-500/5 px-2.5 py-2 text-xs leading-relaxed text-emerald-700">
              <div className="mb-1 flex items-center gap-1 font-medium">
                <Check className="h-3 w-3" />
                sing-box（SFA）一键导入
              </div>
              用 sing-box「添加配置 → 扫描二维码」导入，自动生成 TUN 配置并走 HTTP 代理连接到{" "}
              <code className="font-mono">{connectAddr || "本机"}</code>，无需手动填写。
            </div>
          ) : (
            <div className="w-full rounded-md bg-muted/60 px-2.5 py-2 text-xs leading-relaxed text-muted-foreground">
              手机代理软件（如 sing-box / Clash）添加 HTTP 代理，服务器填{" "}
              <code className="font-mono">{lease.lan_ip || "本机IP"}</code>，端口填{" "}
              <code className="font-mono">{lease.agent_listen_port}</code>。
            </div>
          )}

          <div className="w-full border-t border-border pt-3">
            <div className="flex items-center gap-2 text-xs text-muted-foreground">
              <MonitorSmartphone className="h-3.5 w-3.5" />
              <span className="truncate">
                手机代理填 <span className="font-mono">{connectAddr || "IP:端口"}</span>
              </span>
            </div>
          </div>
        </div>
      </section>
    </>
  );
}
