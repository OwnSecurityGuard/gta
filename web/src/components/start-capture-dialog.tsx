import { useState, useEffect } from "react";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { useStartCapture, useBeginCaptureRun, useRegisteredPlugins, useListInterfaces } from "@/hooks/use-mcp";
import { groupParsers, GROUP_LABEL } from "@/lib/parsers";
import { toast } from "@/components/ui/toast";
import { X, Check, Play, Network, Copy } from "lucide-react";

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

/** 开始抓包对话框：本机网卡抓包 / 远程 agent 推流（移动代理抓包为常驻服务，见「代理服务器配置」页）。 */
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
  const [port, setPort] = useState("8080");
  const [plugin, setPlugin] = useState("");
  // 从项目一键抓包时带入的项目 id（本次抓包会话归属到此项目）。
  const [projectId, setProjectId] = useState("");
  const [started, setStarted] = useState(false);
  // agent 源启动成功后保持弹窗打开：成员机需要带真实会话ID的完整 gta-agent 命令
  // （--session/--iface 缺一则只托管插件、不抓包推流），故展示可复制命令而非自动关窗。
  const [agentCommand, setAgentCommand] = useState<string | null>(null);
  const start = useStartCapture();
  // 抓包成功后自动开启行为窗口：begin_capture_run 会读取 MCP 侧 current.json
  // （start_capture 已在服务端同步写入），从而复用同一 session_id，实现抓取↔窗口联动。
  const begin = useBeginCaptureRun();
  // 已注册且在线才能用于抓包解码；离线插件无法建立解码流，故置灰禁用但保留可见，便于排查。
  const { data: pluginsData } = useRegisteredPlugins();
  const plugins = pluginsData?.plugins ?? [];
  // list_interfaces：抓取网卡列表（仅供参考——当前一次 MCP 服务实例只绑一个 -iface，
  // start_capture 不支持按会话切换网卡，故这里只读展示，帮助用户判断“为何没抓到环回流量”）。
  const { data: ifacesData } = useListInterfaces();
  const interfaces = ifacesData?.interfaces ?? [];
  // 已注册插件归组（Godot/Unity/HTTP/自定义），供普通用户以卡片而非下拉选择解析器。
  const pluginGroups = groupParsers(plugins);

  useEffect(() => {
    if (open) {
      setStarted(false);
      setAgentCommand(null);
      // 打开时应用项目预填：有初始端口/插件才覆盖默认值，否则回到默认。
      if (initialPort && initialPort > 0) setPort(String(initialPort));
      if (initialPlugin) setPlugin(initialPlugin);
      setProjectId(initialProjectId ?? "");
    }
    // 仅在每次打开时读取一次预填（把 initialPort/initialPlugin 当作当次快照）。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  function handleStart() {
    const p = parseInt(port, 10);
    // 仅本机网卡抓包要求端口（BPF 过滤用）；远程 agent 由 agent 侧自行过滤，端口可留空。
    if (source === "nic" && (!p || p <= 0)) return;
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
          // 端口可能为空（agent 源），避免渲染出悬空的「端口 」。
          const detail = [port ? `端口 ${port}` : null, plugin ? `插件 ${plugin}` : null]
            .filter(Boolean)
            .join(" · ");
          toast.success("抓包会话已启动", detail);
          // 自动开启行为窗口，与本次抓包会话联动（start_capture 已写入 current.json）。
          // 仅本机网卡抓包联动（有明确端口可生成 BPF 过滤）；agent 源的窗口由 agent 侧另行开启。
          if (source !== "nic") {
            // agent 抓包推流要求成员机以 --session/--iface 启动 gta-agent：
            // 不自动关窗，展示含真实会话ID的完整命令供复制。
            setStarted(true);
            setAgentCommand(`gta-agent --server <服务端>:9091 --token <令牌> --session ${sessionId} --iface <本机网卡>`);
            return;
          }
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

  // 统一关闭路径：清掉 agent 命令状态再回调（重新打开时 useEffect 亦会兜底重置）。
  function handleClose() {
    setAgentCommand(null);
    onClose();
  }

  return (
    <Dialog
      open={open}
      onClose={handleClose}
      icon={<Play className="h-5 w-5" />}
      title="开始抓包"
      description="本机网卡抓包，或由远程 agent 推流；移动代理抓包为常驻服务，请在「代理服务器配置」中查看连接二维码。"
      footer={
        <>
          <Button variant="outline" onClick={handleClose}>
            <X className="h-4 w-4" />
            取消
          </Button>
          <Button onClick={handleStart} disabled={start.isPending}>
            {started ? (
              <>
                <Check className="h-4 w-4" />
                已启动
              </>
            ) : start.isPending ? (
              "启动中…"
            ) : (
              "启动"
            )}
          </Button>
        </>
      }
    >
      {agentCommand ? (
        // agent 源成功态：展示含真实会话ID的完整启动命令，供成员机复制执行。
        <div className="space-y-3">
          <p className="text-sm">
            会话已创建，请在成员机运行以下命令（agent 将抓取本机流量并推流到此会话）
          </p>
          <div className="rounded-md bg-muted px-2 py-1.5 font-mono text-xs text-foreground break-all">
            {agentCommand}
          </div>
          <div className="flex items-center justify-end gap-2">
            <Button
              variant="outline"
              onClick={() => {
                void navigator.clipboard.writeText(agentCommand);
                toast.success("已复制");
              }}
            >
              <Copy className="h-4 w-4" />
              复制
            </Button>
            <Button onClick={handleClose}>
              <Check className="h-4 w-4" />
              完成
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
                  { id: "agent", label: "远程 agent" },
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
                需在成员机运行{" "}
                <code className="font-mono">
                  gta-agent --server &lt;服务端&gt;:9091 --token &lt;令牌&gt; --session &lt;会话ID&gt; --iface &lt;本机网卡&gt;
                </code>
                ，agent 会抓取本机流量并推流到此会话（端口可留空，若填写端口，仅作记录用途）；会话ID 在启动后可从顶栏复制。
              </p>
            )}
          </div>
          <div>
            <label className="text-sm font-medium">端口</label>
            <Input
              value={port}
              onChange={(e) => setPort(e.target.value)}
              aria-label="监听端口"
              inputMode="numeric"
              placeholder={source === "nic" ? "8080" : "可留空"}
              className="mt-1.5 font-mono"
            />
          </div>
          {/* 可用网卡（只读参考）：捕获网卡在 MCP 启动时由 -iface 固定，不可按会话切换。 */}
          <div>
            <label className="flex items-center gap-1.5 text-sm font-medium">
              <Network className="h-3.5 w-3.5 text-muted-foreground" />
              可用网卡（参考）
            </label>
            {interfaces.length === 0 ? (
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
            )}
            <p className="mt-1 text-xs text-muted-foreground">
              网卡在 MCP 启动时由 <code className="font-mono">-iface</code> 固定，本对话框不切换网卡。
            </p>
          </div>

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

      {start.isError && (
        <p className="mt-3 text-xs text-destructive">
          启动失败：{start.error?.message}
        </p>
      )}
    </Dialog>
  );
}
