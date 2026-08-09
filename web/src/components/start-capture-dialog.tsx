import { useState, useEffect } from "react";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { useStartCapture, useRegisteredPlugins } from "@/hooks/use-mcp";
import { toast } from "@/components/ui/toast";
import { X, Check, Play } from "lucide-react";

interface StartCaptureDialogProps {
  open: boolean;
  onClose: () => void;
  onStarted?: (sessionId: string) => void;
}

/** 开始抓包对话框：指定端口与可选解码插件，启动一次抓包会话。 */
export function StartCaptureDialog({ open, onClose, onStarted }: StartCaptureDialogProps) {
  const [port, setPort] = useState("8080");
  const [plugin, setPlugin] = useState("");
  const [started, setStarted] = useState(false);
  const start = useStartCapture();
  // 已注册且在线才能用于抓包解码；离线插件无法建立解码流，故置灰禁用但保留可见，便于排查。
  const { data: pluginsData } = useRegisteredPlugins();
  const plugins = pluginsData?.plugins ?? [];

  useEffect(() => {
    if (open) setStarted(false);
  }, [open]);

  function handleStart() {
    const p = parseInt(port, 10);
    if (!p || p <= 0) return;
    start.mutate(
      { port: p, plugin: plugin || undefined },
      {
        onSuccess: (data) => {
          if (data?.session_id) onStarted?.(data.session_id);
          toast.success("抓包会话已启动", `端口 ${port}${plugin ? ` · 插件 ${plugin}` : ""}`);
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
          <label className="text-sm font-medium">端口</label>
          <Input
            value={port}
            onChange={(e) => setPort(e.target.value)}
            aria-label="监听端口"
            inputMode="numeric"
            placeholder="8080"
            className="mt-1.5 font-mono"
          />
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
