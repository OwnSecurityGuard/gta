import { useEffect, useState } from "react";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { Server, Radio, Download, X } from "lucide-react";
import { useAgentDownloadOptions, useRegisteredPlugins } from "@/hooks/use-mcp";
import { withTokenParam } from "@/lib/auth";
import { toast } from "@/components/ui/toast";

interface AgentDownloadDialogProps {
  open: boolean;
  onClose: () => void;
}

/** 远程 Agent 下载对话框：为不在同一网络环境的成员生成「免参数」的抓包 agent。
 * 成员只需在本页填抓包端口 + 解码插件，回连地址/端口/token/会话都烧进二进制，
 * 运行产物即可抓包上报，无需任何命令行参数。服务端即时代编译（仅服务端本机平台）。 */
export function AgentDownloadDialog({ open, onClose }: AgentDownloadDialogProps) {
  const { data, isLoading } = useAgentDownloadOptions();
  const { data: pluginsData } = useRegisteredPlugins();
  const plugins = pluginsData?.plugins ?? [];

  const opts = (data ?? null) as null | NonNullable<typeof data>;
  // 端口/插件跟随下载写入二进制；回连地址默认用「当前服务部署 host + registry 端口」，
  // 用户可改为远端 agent 可达的公网/局域网地址（端口必须仍是 registry 端口）。
  const [port, setPort] = useState("8080");
  const [plugin, setPlugin] = useState("");
  const [server, setServer] = useState("");
  const [busy, setBusy] = useState(false);

  // 默认回连 IP：优先取浏览器当前访问的服务端 hostname（容器内的 lanIP 探测结果
  // 常是不可路由的内部网段，不可用）；取不到时退回服务端探测到的 host。
  const deployHost = [
    typeof window !== "undefined" ? window.location.hostname : "",
    opts?.host ?? "",
  ].find((h) => Boolean(h)) ?? "";

  // 打开时用服务端已部署信息预填回连地址（host:registry_port），用户可直接改 host。
  useEffect(() => {
    if (!open) return;
    setBusy(false);
    if (deployHost && opts?.registry_port) {
      setServer(`${deployHost}:${opts.registry_port}`);
    } else {
      setServer("");
    }
  }, [open, deployHost, opts?.registry_port]);

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

  function handleDownload() {
    const p = Number(port.trim());
    if (!Number.isInteger(p) || p <= 0 || p > 65535) {
      toast.error("请填写有效的端口", "1-65535 之间");
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
    // 服务端即时代编译 + 下发二进制；token 随查询参数携带（该端点处于鉴权链中）。
    const url = withTokenParam(
      `/download/agent?port=${encodeURIComponent(p)}&server=${encodeURIComponent(addr)}&plugin=${encodeURIComponent(plugin)}`,
    );
    const a = document.createElement("a");
    a.href = url;
    a.rel = "noopener";
    // 同源下载：服务端返回 Content-Disposition attachment，download 属性仅作兜底命名。
    a.download = `gta-agent-${port}`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    // 下载即会创建接收会话；给出简短提示，无需等待编译完成（浏览器自行续传）。
    setTimeout(() => setBusy(false), 1500);
    toast.success("Agent 下载已触发", `抓包端口 ${port}${plugin ? ` · 插件 ${plugin}` : ""}`);
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      icon={<Download className="h-5 w-5" />}
      title="下载远程 Agent"
      description="为不同环境的成员生成免参数抓包 agent：指定抓包端口与解码插件后，回连地址、token 与会话都一并烧进二进制，运行即可抓包上报。"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            <X className="h-4 w-4" />
            关闭
          </Button>
          <Button onClick={handleDownload} disabled={busy || isLoading || !opts?.host}>
            <Download className="h-4 w-4" />
            {busy ? "已触发…" : "下载 Agent"}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        {/* 服务端已部署信息：暴露当前 IP 与端口，供用户参考/预填。 */}
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
              <span className="text-muted-foreground">服务端平台</span>
              <span>{opts.platform}</span>
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
            Agent 将抓取本机对该端口的 TCP/UDP 流量（自动生成 BPF 过滤）并推送到服务端会话。
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
            端口必须是上面的 registry 端口；IP 需为远端 Agent 可达的地址（默认已填入当前服务部署 IP，可在不同网段时改为公网地址）。
          </p>
        </div>
      </div>
    </Dialog>
  );
}