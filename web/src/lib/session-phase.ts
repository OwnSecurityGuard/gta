// 会话阶段（Session Phase）派生：把后端的状态机翻译成人能理解的状态。
//
// 为什么需要这一层：后端只有 running / stopped / closed 三个生命周期态，
// 它们回答的是「进程在不在」，而用户想知道的是「我该不该再等等、还是要去动什么」。
// 典型例子：agent 已连上但一个包都没有——生命周期上是 running，看起来"在工作"，
// 但用户此刻最需要的是"去启动游戏"。这两件事必须由 UI 说清楚，否则用户只会
// 觉得 GTA 坏了。
//
// 设计原则：
//  1. 后端只报事实（连没连上、收到几个包、解析出几个事件），翻译全在这里做；
//  2. 纯函数、无 React 依赖，可被列表/详情/设备卡片共用，也便于单测；
//  3. 数据缺失（降级态）时保守地报更靠前的阶段，不谎报"已完成"。

import type { SessionInfo } from "@/types/session";
import type { SessionStatusResult } from "@/types/session-extra";

/** 会话对用户呈现的阶段（人话状态，非生命周期态）。 */
export type SessionPhase =
  /** 会话刚创建，抓包链路还没跑起来 */
  | "preparing"
  /** agent 源：链路就绪，但还没有任何 agent 连上来 */
  | "awaiting_agent"
  /** agent 已连上，目标端口还没有流量 */
  | "agent_idle"
  /** 抓包链路在跑（含 agent 已连上），但还没有捕获到包 */
  | "awaiting_traffic"
  /** 有包、但还没有解析出事件（无插件 / 插件还没产出） */
  | "capturing"
  /** 正在产出事件，数据可用 */
  | "decoding"
  /** 已停止且抓到了数据，可以分析 */
  | "analyzable"
  /** 已停止但什么都没抓到 */
  | "empty"
  /** 元数据说在跑，实时态说已关闭——pipeline 重启/会话 stale，数据已冻结 */
  | "interrupted";

/** 进度条单步的状态。 */
export type StepState = "done" | "active" | "pending" | "failed";

/** 进度条单步。 */
export interface PhaseStep {
  key: string;
  label: string;
  state: StepState;
}

/** 派生结果：阶段 + 展示元数据 + 可选的排查指引。 */
export interface SessionPhaseView {
  phase: SessionPhase;
  /** 一句话标题（徽标文字，详情页用） */
  title: string;
  /** 短标题（窄容器如会话列表用，不换行塞得下） */
  shortTitle: string;
  /** 补充说明（当前正在发生什么） */
  detail: string;
  /** 语义色调，决定徽标/提示配色 */
  tone: "progress" | "wait" | "live" | "done" | "warn" | "error";
  /** 进度条步骤（已按来源裁剪：非 agent 源不含「连接 Agent」步） */
  steps: PhaseStep[];
  /** 三行「已连接 ✓ / 抓包中 ✓ / Packets: N」式的事实核对表 */
  facts: { label: string; ok: boolean; value: string }[];
  /** 排查建议（仅在用户需要动手时出现；顺利推进时为 undefined） */
  guidance?: { title: string; steps: string[] };
}

/** 派生输入：会话元数据 + 实时态（两者都可能缺失）。 */
export interface PhaseInput {
  /** list_all_sessions 的会话元数据（停止后唯一数据源） */
  meta?: SessionInfo | null;
  /** get_session_status 实时态（运行中优先） */
  status?: SessionStatusResult | null;
}

/** 来源展示名。 */
function sourceName(source?: string): string {
  if (source === "agent") return "远程 Agent";
  if (source === "proxy") return "移动代理";
  return "服务器网卡";
}

/** 取实时态的包数（gRPC 态与元数据降级态字段不同）。 */
function packetsOf(meta?: SessionInfo | null, status?: SessionStatusResult | null): number {
  const live = (status?.packets_in ?? 0) + (status?.raw_count ?? 0);
  if (live > 0) return live;
  return status?.raw_packets ?? meta?.raw_packets ?? 0;
}

function eventsOf(meta?: SessionInfo | null, status?: SessionStatusResult | null): number {
  return status?.event_count ?? status?.events ?? meta?.events ?? 0;
}

function decodeErrorsOf(meta?: SessionInfo | null, status?: SessionStatusResult | null): number {
  return status?.decode_errors ?? meta?.decode_errors ?? 0;
}

/**
 * 判断会话是否处于「中断」：元数据仍记着 running，实时态却报 closed。
 *
 * 这就是 pipeline 重启后的 session stale——数据已冻结但 UI 若只看元数据会一直
 * 显示「抓包中」，用户以为还在抓，其实什么都不会再进来了。必须单独成态并提示。
 */
export function isInterrupted(meta?: SessionInfo | null, status?: SessionStatusResult | null): boolean {
  if (meta?.status !== "running") return false;
  // 只有明确拿到实时态且它说关了，才判定中断（拿不到实时态时保守起见不算）。
  if (!status?.state) return false;
  return status.state === "closed";
}

/**
 * 派生会话阶段。
 *
 * 判定顺序（先异常、再终止、再运行细分）：
 *  interrupted → stopped(analyzable/empty) → running(awaiting_agent → agent_idle
 *  → awaiting_traffic → capturing → decoding) → preparing
 */
export function deriveSessionPhase({ meta, status }: PhaseInput): SessionPhase {
  const isAgent = (meta?.source ?? status?.source_name) === "agent";
  const running = (status?.state ?? meta?.status) === "running";
  const packets = packetsOf(meta, status);
  const events = eventsOf(meta, status);

  if (isInterrupted(meta, status)) return "interrupted";

  if (!running) {
    // 已停止：有数据即可分析，没数据就是"白抓一场"（empty 是失败态，要给指引）。
    if (packets === 0 && events === 0) return "empty";
    return "analyzable";
  }

  if (isAgent) {
    // agent 源：先确认连上没，再确认有没有流量——两者指引完全不同。
    if (status && !status.agent_connected) return "awaiting_agent";
    if (packets === 0) return "agent_idle";
  } else if (packets === 0) {
    return "awaiting_traffic";
  }

  // 有包无事件：可能是没配插件，也可能插件还没产出——统一叫"正在解析"。
  if (events === 0) return "capturing";
  return "decoding";
}

/** 秒数人话化（"刚刚" / "3 分钟前"）。 */
export function fmtAgo(unixSec?: number): string {
  if (!unixSec) return "";
  const diff = Math.max(0, Math.floor(Date.now() / 1000 - unixSec));
  if (diff < 10) return "刚刚";
  if (diff < 60) return `${diff} 秒前`;
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
  return `${Math.floor(diff / 3600)} 小时前`;
}

const PORT_HINT = "确认游戏/客户端实际连的就是这个端口（netstat 或游戏日志里核对）";

/**
 * 把阶段翻译成完整展示模型（标题 / 进度条 / 事实核对表 / 排查指引）。
 *
 * facts 的意义：用户判断"到底哪个环节没通"只需要三个布尔量——连上没、抓到没、
 * 解析出没。把它们平铺出来比任何监控面板都直接。
 */
export function describeSessionPhase(input: PhaseInput): SessionPhaseView {
  const { meta, status } = input;
  const phase = deriveSessionPhase(input);
  const src = sourceName(meta?.source ?? status?.source_name);
  const isAgent = (meta?.source ?? status?.source_name) === "agent";
  const packets = packetsOf(meta, status);
  const events = eventsOf(meta, status);
  const errors = decodeErrorsOf(meta, status);
  const port = meta?.port ?? status?.port ?? 0;
  const lastSeenAgo = fmtAgo(status?.agent_last_seen_unix);

  // 进度条：非 agent 源把「连接 Agent」换成「数据源就绪」，步骤数保持一致。
  const linkLabel = isAgent ? "连接 Agent" : "连接数据源";
  const steps: PhaseStep[] = [
    { key: "prepare", label: "准备中", state: "pending" },
    { key: "link", label: linkLabel, state: "pending" },
    { key: "traffic", label: "等待流量", state: "pending" },
    { key: "capture", label: "正在抓包", state: "pending" },
    { key: "decode", label: "正在解析", state: "pending" },
    { key: "ready", label: "可分析", state: "pending" },
  ];

  // 事实核对表：三个布尔量足以定位"卡在哪一环"。
  const linked = isAgent ? !!status?.agent_connected : true;
  const facts: SessionPhaseView["facts"] = [
    {
      label: isAgent ? "Agent 已连接" : "数据源就绪",
      ok: linked,
      value: linked ? "✓" : "未连接",
    },
    {
      label: "已抓到包",
      ok: packets > 0,
      value: packets > 0 ? `✓ ${packets.toLocaleString()}` : `Packets: 0`,
    },
    {
      label: "已解析事件",
      ok: events > 0,
      value: events > 0 ? `✓ ${events.toLocaleString()}` : `Events: 0`,
    },
  ];

  // mark：把 until 之前（不含）的步骤统一置为某状态；setStep：单独置一步。
  const mark = (until: string, state: StepState) => {
    for (const s of steps) {
      if (s.key === until) break;
      s.state = state;
    }
  };
  const setStep = (key: string, state: StepState) => {
    const s = steps.find((x) => x.key === key);
    if (s) s.state = state;
  };

  switch (phase) {
    case "preparing":
      mark("prepare", "done");
      setStep("prepare", "active");
      return {
        phase,
        title: "准备中",
        shortTitle: "准备中",
        detail: `正在建立${src}抓包链路`,
        tone: "progress",
        steps,
        facts,
      };

    case "awaiting_agent":
      mark("link", "done");
      setStep("link", "active");
      return {
        phase,
        title: "等待 Agent 接入",
        shortTitle: "等待 Agent",
        detail: "抓包会话已就绪，但还没有 Agent 连上来",
        tone: "wait",
        steps,
        facts,
        guidance: {
          title: "还没有 Agent 接入。请：",
          steps: [
            "在目标电脑上下载并运行 GTA Agent（用上面的启动码）",
            "确认 Agent 与 GTA 服务端网络互通（防火墙放行 ingest 端口）",
            "Agent 连上后这里会自动变成「等待流量」，无需刷新",
          ],
        },
      };

    case "agent_idle":
      mark("traffic", "done");
      setStep("traffic", "active");
      return {
        phase,
        title: "Agent 已连接，等待流量",
        shortTitle: "已连接·无流量",
        detail: "链路正常，但目标端口还没有产生任何数据",
        tone: "wait",
        steps,
        facts,
        guidance: {
          title: "Agent 已连接，但还没有捕获到数据。请：",
          steps: [
            "启动游戏 / 客户端",
            "在游戏里执行一次会产生网络请求的操作（登录、进入房间、移动）",
            port > 0
              ? `确认目标端口 ${port} 正在产生流量（${PORT_HINT}）`
              : PORT_HINT,
          ],
        },
      };

    case "awaiting_traffic":
      mark("traffic", "done");
      setStep("traffic", "active");
      return {
        phase,
        title: "正在等待流量",
        shortTitle: "等待流量",
        detail: `${src}抓包链路运行中，尚未捕获到数据`,
        tone: "wait",
        steps,
        facts,
        guidance: {
          title: "还没有捕获到数据。请：",
          steps: [
            "确认游戏 / 客户端已经启动并正在联网",
            "执行一次会产生网络请求的操作",
            port > 0 ? `确认端口 ${port} 正在产生流量（${PORT_HINT}）` : PORT_HINT,
          ],
        },
      };

    case "capturing":
      mark("capture", "done");
      setStep("capture", "active");
      return {
        phase,
        title: "正在抓包",
        shortTitle: "抓包中",
        detail: meta?.plugin
          ? `已收到 ${packets.toLocaleString()} 个包，等待 ${meta.plugin} 解析`
          : `已收到 ${packets.toLocaleString()} 个包（未指定解析插件，仅抓包不解码）`,
        tone: "live",
        steps,
        facts,
        // 有包无事件且没配插件：这是配置问题，不是故障，但用户得知道。
        guidance: meta?.plugin
          ? undefined
          : {
              title: "当前没有配置解析插件，只会抓包不会解码。",
              steps: ["停止抓包后重新选择解析插件，或用「切换插件」热切换"],
            },
      };

    case "decoding":
      mark("decode", "done");
      setStep("decode", "active");
      return {
        phase,
        title: "正在解析",
        shortTitle: "解析中",
        detail: `已解析 ${events.toLocaleString()} 个事件${errors > 0 ? ` · ${errors.toLocaleString()} 条解码失败` : ""}`,
        tone: "live",
        steps,
        facts,
        guidance:
          errors > 0
            ? {
                title: "部分数据无法解析。",
                steps: [
                  "可能是非目标协议流量混入，或解析插件与协议版本不匹配",
                  "到「协议数据」页看失败样本，确认插件是否选对",
                ],
              }
            : undefined,
      };

    case "analyzable":
      mark("ready", "done");
      setStep("ready", "done");
      return {
        phase,
        title: "可分析",
        shortTitle: "可分析",
        detail: `抓包已结束：${packets.toLocaleString()} 个包 / ${events.toLocaleString()} 个事件`,
        tone: "done",
        steps,
        facts,
        guidance:
          errors > 0
            ? {
                title: "本次有解码失败数据。",
                steps: [
                  `${errors.toLocaleString()} 条解码失败，可能是非目标协议流量或插件不匹配`,
                  "到「协议数据」页核对样本，必要时换插件重新抓",
                ],
              }
            : undefined,
      };

    case "empty":
      // 失败态：明确告诉用户"没抓到"，并列出该做什么，而不是只显示一个 0。
      mark("traffic", "done");
      setStep("traffic", "failed");
      setStep("capture", "pending");
      return {
        phase,
        title: "没抓到数据",
        shortTitle: "无数据",
        detail: "本次抓包已结束，但没有捕获到任何流量",
        tone: "error",
        steps,
        facts,
        guidance: {
          title: "这次什么都没抓到。下次请：",
          steps: [
            "先启动游戏 / 客户端，再开始抓包",
            "抓包期间执行一次会产生网络请求的操作",
            port > 0
              ? `核对抓包端口 ${port} 与游戏实际端口一致（${PORT_HINT}）`
              : PORT_HINT,
            isAgent ? "确认 Agent 期间没有断开（断开期间的数据不会补发）" : "确认网卡选择正确（多网卡机器极易选错）",
          ],
        },
      };

    case "interrupted":
      mark("capture", "done");
      setStep("capture", "failed");
      return {
        phase,
        title: "抓包已中断",
        shortTitle: "已中断",
        detail: `会话仍标记为运行中，但抓包服务已不再持有它${lastSeenAgo ? `（最后收到数据：${lastSeenAgo}）` : ""}`,
        tone: "warn",
        steps,
        facts,
        guidance: {
          title: "这通常发生在 GTA 服务重启后。",
          steps: [
            "已抓到的数据是完整可用的，可以直接分析",
            "要继续抓包请新建一次会话（旧会话不会被复用）",
          ],
        },
      };
  }
}
