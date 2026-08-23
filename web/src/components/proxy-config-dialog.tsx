import { useEffect, useRef, useState } from "react";
import QRCode from "react-qr-code";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Dialog } from "@/components/ui/dialog";
import { useProxyServerConfig, useUpdateProxyServerConfig, useListPlugins } from "@/hooks/use-mcp";
import { toast } from "@/components/ui/toast";
import { Cable, Check, Copy, RefreshCw, X, Server } from "lucide-react";

interface ProxyConfigDialogProps {
  open: boolean;
  onClose: () => void;
}

/** 代理抓包服务器配置对话框：查看/修改服务器配置 + 生成手机扫码连接的二维码。
 * 代理抓包为常驻服务（无需手动开始抓包），本页只负责"服务器侧"配置与二维码分发。 */
export function ProxyConfigDialog({ open, onClose }: ProxyConfigDialogProps) {
  const { data, isLoading, refetch } = useProxyServerConfig();
  const update = useUpdateProxyServerConfig();
  const plugins = useListPlugins();
  const state = data?.state;

  // 表单本地状态；服务端配置仅在"未编辑过"时同步，避免 5s 轮询覆盖用户正在输入的内容。
  const [listenAddr, setListenAddr] = useState("");
  const [serverAddr, setServerAddr] = useState("");
  const [frameStyle, setFrameStyle] = useState<"raw" | "length_prefix">("raw");
  const [prefixLen, setPrefixLen] = useState("4");
  const [littleEndian, setLittleEndian] = useState<"false" | "true">("false");
  const [plugin, setPlugin] = useState("");
  const [filterHosts, setFilterHosts] = useState("");
  const [filterPorts, setFilterPorts] = useState("");
  const [saved, setSaved] = useState(false);
  const dirtyRef = useRef(false);
  const syncedKeyRef = useRef("");

  // 打开弹窗时重置"已编辑"标记，并从最新配置初始化表单。
  useEffect(() => {
    if (!open) return;
    dirtyRef.current = false;
    syncedKeyRef.current = "";
  }, [open]);

  // 服务端配置到达/变化时，若用户未编辑则同步到表单（首次加载、保存成功后均走这里）。
  useEffect(() => {
    if (!state || dirtyRef.current) return;
    const key = JSON.stringify([
      state.listen_addr,
      state.server_addr,
      state.frame_style,
      state.prefix_len,
      state.little_endian,
      state.plugin,
      state.include_hosts,
      state.include_ports,
    ]);
    if (key === syncedKeyRef.current) return;
    syncedKeyRef.current = key;
    setListenAddr(state.listen_addr);
    setServerAddr(state.server_addr);
    setFrameStyle(state.frame_style === "length_prefix" ? "length_prefix" : "raw");
    setPrefixLen(String(state.prefix_len || 4));
    setLittleEndian(state.little_endian ? "true" : "false");
    setPlugin(state.plugin ?? "");
    setFilterHosts((state.include_hosts ?? []).join(","));
    setFilterPorts((state.include_ports ?? []).join(","));
  }, [state]);

  const connectAddr = state?.connect_addr ?? "";
  const singboxUri = state?.singbox_uri ?? "";
  const agentUp = !!state?.agent_running;
  const sessionUp = !!state?.session_running;
  const pluginOptions = plugins.data?.plugins ?? [];

  function handleSave() {
    update.mutate(
      {
        listenAddr: listenAddr.trim(),
        serverAddr: serverAddr.trim(),
        frameStyle,
        prefixLen: frameStyle === "length_prefix" ? parseInt(prefixLen, 10) : undefined,
        littleEndian: frameStyle === "length_prefix" ? littleEndian === "true" : undefined,
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
          if (st) {
            syncedKeyRef.current = JSON.stringify([
              st.listen_addr,
              st.server_addr,
              st.frame_style,
              st.prefix_len,
              st.little_endian,
              st.plugin,
              st.include_hosts,
              st.include_ports,
            ]);
            setListenAddr(st.listen_addr);
            setServerAddr(st.server_addr);
            setFrameStyle(st.frame_style === "length_prefix" ? "length_prefix" : "raw");
            setPrefixLen(String(st.prefix_len || 4));
            setLittleEndian(st.little_endian ? "true" : "false");
            setPlugin(st.plugin ?? "");
            setFilterHosts((st.include_hosts ?? []).join(","));
            setFilterPorts((st.include_ports ?? []).join(","));
          }
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

  async function handleCopy() {
    const text = singboxUri || connectAddr;
    if (!text) return;
    try {
      await navigator.clipboard.writeText(text);
      toast.success("已复制导入链接", text);
    } catch {
      toast.info("复制失败，请手动选择地址", text);
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      icon={<Cable className="h-5 w-5" />}
      title="代理服务器配置"
      description="代理抓包为常驻服务，无需手动开始抓包。配置服务器并生成二维码，手机代理软件扫码即可连接。"
      className="max-w-2xl"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            <X className="h-4 w-4" />
            关闭
          </Button>
          <Button onClick={handleSave} disabled={update.isPending || isLoading}>
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
      <div className="grid gap-5 sm:grid-cols-[1fr_auto]">
        {/* 左：配置表单 */}
        <div className="space-y-3">
          <div>
            <label className="text-sm font-medium">代理监听地址（手机连接）</label>
            <Input
              value={listenAddr}
              onChange={(e) => {
                dirtyRef.current = true;
                setListenAddr(e.target.value);
              }}
              aria-label="代理监听地址"
              placeholder="0.0.0.0:12000"
              className="mt-1.5 font-mono"
            />
            <p className="mt-1 text-xs text-muted-foreground">
              gta-singbox-agent 的 HTTP CONNECT 代理监听地址。手机经局域网访问时必须绑定{" "}
              <code className="font-mono">0.0.0.0</code> 或具体局域网 IP。
            </p>
          </div>

          <div>
            <label className="text-sm font-medium">数据服务地址（agent 推送）</label>
            <Input
              value={serverAddr}
              onChange={(e) => {
                dirtyRef.current = true;
                setServerAddr(e.target.value);
              }}
              aria-label="数据服务地址"
              placeholder="127.0.0.1:9090"
              className="mt-1.5 font-mono"
            />
            <p className="mt-1 text-xs text-muted-foreground">
              mobile Source（常驻抓包会话）的 gRPC 监听地址，agent 推送连接级数据到这里。
            </p>
          </div>

          <div>
            <label className="text-sm font-medium">分帧方式</label>
            <select
              className="mt-1.5 h-9 w-full rounded-md border border-input bg-background px-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
              value={frameStyle}
              onChange={(e) => {
                dirtyRef.current = true;
                setFrameStyle(e.target.value as "raw" | "length_prefix");
              }}
            >
              <option value="raw">raw（每个数据块一帧，解码器自行处理粘包）</option>
              <option value="length_prefix">length_prefix（前 N 字节长度前缀分帧）</option>
            </select>
          </div>

          {frameStyle === "length_prefix" && (
            <div className="grid grid-cols-2 gap-3">
              <div>
                <label className="text-sm font-medium">长度前缀字节数</label>
                <select
                  className="mt-1.5 h-9 w-full rounded-md border border-input bg-background px-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
                  value={prefixLen}
                  onChange={(e) => {
                    dirtyRef.current = true;
                    setPrefixLen(e.target.value);
                  }}
                >
                  <option value="1">1 字节</option>
                  <option value="2">2 字节</option>
                  <option value="4">4 字节</option>
                </select>
              </div>
              <div>
                <label className="text-sm font-medium">字节序</label>
                <select
                  className="mt-1.5 h-9 w-full rounded-md border border-input bg-background px-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
                  value={littleEndian}
                  onChange={(e) => {
                    dirtyRef.current = true;
                    setLittleEndian(e.target.value as "false" | "true");
                  }}
                >
                  <option value="false">大端（Big-Endian）</option>
                  <option value="true">小端（Little-Endian）</option>
                </select>
              </div>
            </div>
          )}

          {/* 解码插件 */}
          <div>
            <label className="text-sm font-medium">解码插件</label>
            <select
              className="mt-1.5 h-9 w-full rounded-md border border-input bg-background px-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
              value={plugin}
              onChange={(e) => {
                dirtyRef.current = true;
                setPlugin(e.target.value);
              }}
            >
              <option value="">仅抓原始包（不解码）</option>
              {pluginOptions.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </select>
            <p className="mt-1 text-xs text-muted-foreground">
              代理抓包会话绑定该插件解码原始流量，生成协议事件与连接详情。需插件已注册，空为仅抓原始包。
            </p>
          </div>

          {/* 连接筛选 */}
          <div className="grid gap-3 sm:grid-cols-2">
            <div>
              <label className="text-sm font-medium">筛选目标主机（逗号分隔）</label>
              <Input
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
              <label className="text-sm font-medium">筛选目标端口（逗号分隔）</label>
              <Input
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

          {/* 运行时状态 */}
          <div className="flex flex-wrap items-center gap-2 pt-1">
            <span className="text-xs text-muted-foreground">运行时状态：</span>
            <Badge variant={agentUp ? "default" : "destructive"}>
              <Server className="h-3 w-3" />
              {agentUp ? `Agent 运行中${state?.agent_pid ? ` · PID ${state.agent_pid}` : ""}` : "Agent 未运行"}
            </Badge>
            <Badge variant={sessionUp ? "secondary" : "destructive"}>
              {sessionUp ? "抓包会话运行中" : "抓包会话未运行"}
            </Badge>
            <Button variant="ghost" size="icon" className="ml-auto h-7 w-7" onClick={() => void refetch()} title="刷新状态" aria-label="刷新状态">
              <RefreshCw className="h-3.5 w-3.5" />
            </Button>
          </div>

          {update.isError && (
            <p className="text-xs text-destructive">应用失败：{update.error?.message}</p>
          )}
        </div>

        {/* 右：二维码 + 连接信息 */}
        <div className="flex w-full flex-col items-center gap-3 rounded-xl border border-border bg-muted/40 p-4 sm:w-auto">
          <div className="rounded-lg bg-white p-3 shadow-sm">
            {isLoading ? (
              <div className="flex h-40 w-40 items-center justify-center text-xs text-muted-foreground">
                加载中…
              </div>
            ) : singboxUri ? (
              <QRCode value={singboxUri} size={160} bgColor="#ffffff" fgColor="#0f172a" />
            ) : connectAddr ? (
              <QRCode value={connectAddr} size={160} bgColor="#ffffff" fgColor="#0f172a" />
            ) : (
              <div className="flex h-40 w-40 items-center justify-center text-center text-xs text-muted-foreground">
                暂无局域网地址，无法生成二维码
              </div>
            )}
          </div>
          <div className="w-full text-center">
            <p className="text-xs text-muted-foreground">
              {singboxUri ? "sing-box 扫码自动导入配置" : "手机代理软件扫描即连"}
            </p>
            <button
              type="button"
              onClick={() => void handleCopy()}
              className="mt-1 inline-flex max-w-full items-center gap-1 rounded-md border border-border bg-background px-2 py-1 font-mono text-xs text-foreground transition-colors hover:border-primary/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40"
              title={singboxUri ? "复制 sing-box 导入链接" : "复制连接地址"}
            >
              <Copy className="h-3 w-3 shrink-0 text-muted-foreground" />
              <span className="truncate">{singboxUri || connectAddr || "——"}</span>
            </button>
            {singboxUri ? (
              <p className="mt-2 text-[11px] leading-relaxed text-muted-foreground">
                用 sing-box（SFA）「添加配置 → 扫描二维码」导入，自动生成 TUN 配置并走
                HTTP 代理连接到 <code className="mx-0.5 font-mono">{connectAddr || "本机"}</code>
                ，无需手动填写。
              </p>
            ) : (
              <p className="mt-2 text-[11px] leading-relaxed text-muted-foreground">
                手机代理软件（如 sing-box / Clash）添加 HTTP 代理，服务器填
                <code className="mx-0.5 font-mono">{state?.lan_ip || "本机IP"}</code>，
                端口填代理端口。二维码内容即{" "}
                <code className="font-mono">{connectAddr || "IP:端口"}</code>。
              </p>
            )}
          </div>
        </div>
      </div>
    </Dialog>
  );
}
