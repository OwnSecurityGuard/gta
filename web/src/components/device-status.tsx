import { Laptop, Loader2, Activity, CircleCheck, ChevronRight, Clock } from "lucide-react";
import { useMyDevices } from "@/hooks/use-devices";
import type { DeviceState, DeviceView } from "@/types/device";

const STATE_META: Record<DeviceState, { label: string; text: string }> = {
  waiting: { label: "等待接入", text: "text-amber-600 dark:text-amber-400" },
  connected: { label: "已连接", text: "text-blue-600 dark:text-blue-400" },
  capturing: { label: "正在抓包", text: "text-emerald-600 dark:text-emerald-400" },
  stopped: { label: "已停止", text: "text-muted-foreground" },
};

function platformLabel(platform?: string): string {
  if (!platform) return "";
  const [os, arch] = platform.split("/");
  const osLabel = os === "windows" ? "Windows" : os === "linux" ? "Linux" : os === "darwin" ? "macOS" : os;
  const archLabel = arch === "amd64" ? "AMD64" : arch === "arm64" ? "ARM64" : arch;
  return [osLabel, archLabel].filter(Boolean).join(" · ");
}

function fmtSeen(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const diffMs = Date.now() - d.getTime();
  if (diffMs < 60_000) return "刚刚";
  if (diffMs < 3_600_000) return `${Math.floor(diffMs / 60_000)} 分钟前`;
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  return `今天 ${hh}:${mm}`;
}

function StatusIcon({ state }: { state: DeviceState }) {
  if (state === "waiting") return <Loader2 className="h-4 w-4 animate-spin" />;
  if (state === "capturing") return <Activity className="h-4 w-4" />;
  if (state === "connected") return <CircleCheck className="h-4 w-4" />;
  return <Clock className="h-4 w-4" />;
}

interface DeviceStatusListProps {
  /** 已停止/正在抓包的设备，点击可跳转到对应会话（首页传入；弹窗内可省略）。 */
  onSelectSession?: (sessionId: string) => void;
}

/**
 * 「我的设备」状态列表：把启动码/会话捏成用户能一眼看懂的设备接入闭环。
 * 无任何设备时给出引导文案，而非空白。
 */
export function DeviceStatusList({ onSelectSession }: DeviceStatusListProps) {
  const devices = useMyDevices();

  if (devices.length === 0) {
    return (
      <div className="rounded-xl border border-dashed border-border bg-card/40 px-4 py-5 text-center">
        <Laptop className="mx-auto h-5 w-5 text-muted-foreground/60" />
        <p className="mt-2 text-sm text-muted-foreground">还没有接入设备</p>
        <p className="mt-0.5 text-xs text-muted-foreground/80">
          生成接入命令并在你的电脑上执行，接入后这里会显示「已连接」。
        </p>
      </div>
    );
  }

  return (
    <div className="space-y-2">
      {devices.map((d) => (
        <DeviceCard key={d.code} device={d} onSelectSession={onSelectSession} />
      ))}
    </div>
  );
}

function DeviceCard({
  device,
  onSelectSession,
}: {
  device: DeviceView;
  onSelectSession?: (sessionId: string) => void;
}) {
  const meta = STATE_META[device.state];
  const seen = fmtSeen(device.lastSeen);
  const clickable = !!onSelectSession && !!device.sessionId;

  const body = (
    <div
      className={
        "flex w-full items-center gap-3 rounded-xl border bg-card/60 px-3 py-2.5 text-left transition-colors " +
        (clickable ? "hover:border-primary/30 hover:bg-muted/40" : "border-border")
      }
    >
      <Laptop className="h-5 w-5 shrink-0 text-muted-foreground" />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className={`flex items-center gap-1.5 text-sm font-medium ${meta.text}`}>
            <StatusIcon state={device.state} />
            {meta.label}
          </span>
          {device.platform && (
            <span className="rounded bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
              {platformLabel(device.platform)}
            </span>
          )}
        </div>
        <p className="mt-0.5 truncate text-[11px] text-muted-foreground">
          <span className="font-mono">{device.code}</span>
          {device.port ? ` · 端口 ${device.port}` : ""}
          {device.plugin ? ` · 解析器 ${device.plugin}` : ""}
        </p>
        <p className="mt-0.5 text-[11px] text-muted-foreground">
          {device.state === "capturing" ? (
            <>
              <span className="font-mono tabular-nums">{device.packets?.toLocaleString() ?? 0}</span> packets ·{" "}
              <span className="font-mono tabular-nums">{device.events?.toLocaleString() ?? 0}</span> events
              {device.decodeErrors > 0 ? (
                <>
                  {" "}
                  · <span className="text-red-600 dark:text-red-400">{device.decodeErrors} 解码错误</span>
                </>
              ) : (
                ""
              )}
              {seen ? ` · ${seen}` : ""}
            </>
          ) : device.state === "stopped" ? (
            <>
              最近抓包：{seen || "—"}
              {device.packets > 0
                ? ` · ${device.packets?.toLocaleString() ?? 0} packets / ${device.events?.toLocaleString() ?? 0} events`
                : ""}
            </>
          ) : device.state === "connected" ? (
            <>已建立连接，等待收到网络包{seen ? ` · ${seen}` : ""}</>
          ) : (
            <>启动命令已生成，请在目标电脑执行命令接入</>
          )}
        </p>
      </div>
      {clickable && (
        <span className="inline-flex shrink-0 items-center gap-0.5 rounded-md border border-border px-2 py-1 text-xs text-foreground">
          查看
          <ChevronRight className="h-3 w-3 text-muted-foreground" />
        </span>
      )}
    </div>
  );

  if (!clickable) return body;

  return (
    <button type="button" className="block w-full" onClick={() => onSelectSession?.(device.sessionId!)}>
      {body}
    </button>
  );
}
