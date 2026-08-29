import { useState, useEffect } from "react";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { useStartCapture, useBeginCaptureRun, useRegisteredPlugins, useListInterfaces } from "@/hooks/use-mcp";
import { toast } from "@/components/ui/toast";
import { X, Check, Play, Network } from "lucide-react";

interface StartCaptureDialogProps {
  open: boolean;
  onClose: () => void;
  onStarted?: (sessionId: string) => void;
  /** 抓包启动并自动开启行为窗口后回调，携带 run_id 与联动的 session_id。 */
  onRunLinked?: (runId: string, sessionId: string) => void;
}

/** 开始抓包对话框：本机网卡抓包 / 远程 agent 推流（移动代理抓包为常驻服务，见「代理服务器配置」页）。 */
export function StartCaptureDialog({ open, onClose, onStarted, onRunLinked }: StartCaptureDialogProps) {
  const [source, setSource] = useState<"nic" | "agent">("nic");
  const [port, setPort] = useState("8080");
  const [plugin, setPlugin] = useState("");
  const [started, setStarted] = useState(false);
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

  useEffect(() => {
    if (open) setStarted(false);
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
      },
      {
        onSuccess: (data) => {
          const sessionId = data?.session_id ?? "";
          if (sessionId) onStarted?.(sessionId);
          toast.success("抓包会话已启动", `端口 ${port}${plugin ? ` · 插件 ${plugin}` : ""}`);
          // 自动开启行为窗口，与本次抓包会话联动（start_capture 已写入 current.json）。
          // 仅本机网卡抓包联动（有明确端口可生成 BPF 过滤）；agent 源的窗口由 agent 侧另行开启。
          if (source !== "nic") {
            setStarted(true);
            setTimeout(() => {
              setStarted(false);
              onClose();
            }, 800);
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

  return (
    <Dialog
      open={open}
      onClose={onClose}
      icon={<Play className="h-5 w-5" />}
      title="开始抓包"
      description="本机网卡抓包，或由远程 agent 推流；移动代理抓包为常驻服务，请在「代理服务器配置」中查看连接二维码。"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
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
      <div className="space-y-3">
        <div>
          <label className="text-sm font-medium">抓包源</label>
          <div className="mt-1.5 flex items-center gap-1 rounded-lg bg-muted p-1">
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
              需在成员机运行 <code className="font-mono">gta-agent --server &lt;服务端&gt;:9091 --token &lt;令牌&gt;</code>
              ，agent 会抓取本机流量并推流到此会话（端口可留空）。
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
          <label className="text-sm font-medium">解码插件（可选）</label>
          <select
            className="mt-1.5 h-9 w-full rounded-md border border-input bg-background px-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
            value={plugin}
            onChange={(e) => setPlugin(e.target.value)}
          >
            <option value="">不指定（仅抓包）</option>
            {plugins.map((p) => (
              <option key={p.instance_id} value={p.name} disabled={!p.online}>
                {p.name}（{p.protocol}）{p.online ? "" : " — 离线"}
              </option>
            ))}
          </select>
          <p className="mt-1 text-xs text-muted-foreground">
            {plugins.length === 0
              ? "当前没有已注册的插件，可留空仅抓包，或先启动插件使其注册到 Pipeline。"
              : "留空则只抓包存储原始包；指定插件可在抓包同时解码协议事件（仅在线插件可选）。"}
          </p>
        </div>
      </div>

      {start.isError && (
        <p className="mt-3 text-xs text-destructive">
          启动失败：{start.error?.message}
        </p>
      )}
    </Dialog>
  );
}
