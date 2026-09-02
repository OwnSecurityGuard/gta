// 会话阶段展示：进度条 + 事实核对表 + 排查指引。
//
// 存在的理由：后端只会说 running / stopped / closed，用户需要知道的是
// 「现在到哪一步了、我该不该动手」。本组件把 lib/session-phase 的派生结果
// 画出来，重点服务一个高频场景——「Agent 已连接，但 Packets: 0」：
// 这不是故障，UI 必须明确说清"链路是通的，是你还没产生流量"，并给出三步动作。
import { Check, Circle, Loader2, AlertTriangle, X, Lightbulb, Radio } from "lucide-react";
import { cn } from "@/lib/utils";
import {
  describeSessionPhase,
  type PhaseInput,
  type PhaseStep,
  type SessionPhaseView,
} from "@/lib/session-phase";

/** 阶段色调 → 徽标配色。 */
const TONE_BADGE: Record<SessionPhaseView["tone"], string> = {
  progress: "border-sky-500/40 bg-sky-500/10 text-sky-700 dark:text-sky-300",
  wait: "border-amber-500/50 bg-amber-500/10 text-amber-700 dark:text-amber-300",
  live: "border-emerald-500/40 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
  done: "border-border bg-muted/50 text-foreground",
  warn: "border-orange-500/50 bg-orange-500/10 text-orange-700 dark:text-orange-300",
  error: "border-red-500/50 bg-red-500/10 text-red-700 dark:text-red-300",
};

function StepIcon({ state }: { state: PhaseStep["state"] }) {
  if (state === "done") return <Check className="h-3 w-3" />;
  if (state === "active") return <Loader2 className="h-3 w-3 animate-spin" />;
  if (state === "failed") return <X className="h-3 w-3" />;
  return <Circle className="h-2 w-2" />;
}

function StepDot({ step }: { step: PhaseStep }) {
  const tone =
    step.state === "done"
      ? "border-emerald-500/60 bg-emerald-500/15 text-emerald-600 dark:text-emerald-400"
      : step.state === "active"
        ? "border-primary bg-primary/15 text-primary"
        : step.state === "failed"
          ? "border-red-500/60 bg-red-500/15 text-red-600 dark:text-red-400"
          : "border-border bg-background text-muted-foreground/50";
  return (
    <span
      className={cn(
        "flex h-4 w-4 shrink-0 items-center justify-center rounded-full border",
        tone,
      )}
    >
      <StepIcon state={step.state} />
    </span>
  );
}

/** 水平进度条：准备中 → 连接 Agent → 等待流量 → 抓包中 → 解析中 → 可分析。 */
function Stepper({ steps }: { steps: PhaseStep[] }) {
  return (
    <ol className="flex flex-wrap items-center gap-x-1.5 gap-y-2">
      {steps.map((s, i) => (
        <li key={s.key} className="flex items-center gap-1.5">
          {i > 0 && <span className="mr-0.5 h-px w-3 bg-border" aria-hidden />}
          <span className="flex items-center gap-1.5">
            <StepDot step={s} />
            <span
              className={cn(
                "text-[11px] whitespace-nowrap",
                s.state === "pending"
                  ? "text-muted-foreground/70"
                  : s.state === "active"
                    ? "font-medium text-foreground"
                    : s.state === "failed"
                      ? "text-red-600 dark:text-red-400"
                      : "text-muted-foreground",
              )}
            >
              {s.label}
            </span>
          </span>
        </li>
      ))}
    </ol>
  );
}

/** 事实核对表：三个布尔量定位"卡在哪一环"，比任何监控面板都直接。 */
function FactList({ facts }: { facts: SessionPhaseView["facts"] }) {
  return (
    <dl className="flex flex-wrap gap-x-5 gap-y-1.5">
      {facts.map((f) => (
        <div key={f.label} className="flex items-baseline gap-1.5">
          <dt className="text-[11px] text-muted-foreground">{f.label}</dt>
          <dd
            className={cn(
              "font-mono text-xs tabular-nums",
              f.ok ? "text-emerald-600 dark:text-emerald-400" : "text-foreground",
            )}
          >
            {f.value}
          </dd>
        </div>
      ))}
    </dl>
  );
}

/** 排查指引：只在用户需要动手时出现，顺利推进时不占版面。 */
function Guidance({ guidance }: { guidance: NonNullable<SessionPhaseView["guidance"]> }) {
  return (
    <div className="rounded-xl border border-border bg-muted/40 px-3 py-2.5">
      <p className="flex items-start gap-1.5 text-xs font-medium text-foreground">
        <Lightbulb className="mt-0.5 h-3.5 w-3.5 shrink-0 text-amber-500" />
        {guidance.title}
      </p>
      <ol className="mt-1.5 space-y-1 pl-5">
        {guidance.steps.map((s, i) => (
          <li key={i} className="list-decimal text-xs leading-relaxed text-muted-foreground">
            {s}
          </li>
        ))}
      </ol>
    </div>
  );
}

/** 状态徽标（概览页头部、列表行共用）。 */
export function PhaseBadge({
  input,
  short,
  className,
}: {
  input: PhaseInput;
  /** 窄容器（会话列表）用短标题，避免撑破布局 */
  short?: boolean;
  className?: string;
}) {
  const view = describeSessionPhase(input);
  const spinning = view.tone === "progress" || view.tone === "wait" || view.tone === "live";
  return (
    <span
      // 完整标题放进 title，窄容器里短标题不够看时悬停可得全貌。
      title={view.detail}
      className={cn(
        "inline-flex items-center gap-1 rounded-md border px-2 py-0.5 text-xs font-medium",
        TONE_BADGE[view.tone],
        className,
      )}
    >
      {spinning ? (
        <Radio className="h-3 w-3 animate-pulse" />
      ) : view.tone === "error" || view.tone === "warn" ? (
        <AlertTriangle className="h-3 w-3" />
      ) : (
        <Check className="h-3 w-3" />
      )}
      {short ? view.shortTitle : view.title}
    </span>
  );
}

interface SessionPhaseTrackerProps {
  /** 会话元数据 + 实时态 */
  input: PhaseInput;
  /** 紧凑模式：只给事实表 + 指引，不画进度条（弹窗/侧栏场景） */
  compact?: boolean;
}

/**
 * 会话阶段追踪器。
 *
 * 完整模式（概览页）：进度条 + 事实表 + 指引。
 * 紧凑模式（摘要卡片）：事实表 + 指引，省掉进度条。
 */
export function SessionPhaseTracker({ input, compact }: SessionPhaseTrackerProps) {
  const view = describeSessionPhase(input);
  return (
    <div className="space-y-2.5">
      {!compact && <Stepper steps={view.steps} />}
      <FactList facts={view.facts} />
      {view.guidance && <Guidance guidance={view.guidance} />}
    </div>
  );
}
