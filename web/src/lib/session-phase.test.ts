import { describe, it, expect } from "vitest";
import {
  deriveSessionPhase,
  describeSessionPhase,
  isInterrupted,
  fmtAgo,
} from "@/lib/session-phase";
import type { SessionInfo } from "@/types/session";
import type { SessionStatusResult } from "@/types/session-extra";

function meta(over: Partial<SessionInfo> = {}): SessionInfo {
  return {
    session_id: "s1",
    started_at: new Date().toISOString(),
    stopped_at: "",
    status: "running",
    port: 9250,
    plugin: "godot-ecs",
    interface: "",
    pcap_file: "",
    source: "agent",
    listen_addr: "",
    raw_packets: 0,
    events: 0,
    metrics: 0,
    decode_errors: 0,
    duration_sec: 0,
    db_path: "",
    ...over,
  };
}

function status(over: Partial<SessionStatusResult> = {}): SessionStatusResult {
  return { session_id: "s1", state: "running", ...over };
}

describe("deriveSessionPhase", () => {
  it("agent 源未连上时是 awaiting_agent（不是「抓包中」）", () => {
    expect(deriveSessionPhase({ meta: meta(), status: status({ agent_connected: false }) })).toBe(
      "awaiting_agent",
    );
  });

  it("agent 已连上但零包数是 agent_idle —— 用户最需要的那个状态", () => {
    expect(
      deriveSessionPhase({ meta: meta(), status: status({ agent_connected: true, raw_count: 0 }) }),
    ).toBe("agent_idle");
  });

  it("有包无事件是 capturing", () => {
    expect(
      deriveSessionPhase({ meta: meta(), status: status({ agent_connected: true, raw_count: 120 }) }),
    ).toBe("capturing");
  });

  it("有事件是 decoding", () => {
    expect(
      deriveSessionPhase({
        meta: meta(),
        status: status({ agent_connected: true, raw_count: 120, event_count: 30 }),
      }),
    ).toBe("decoding");
  });

  it("已停止且有数据是可分析，无数据是 empty", () => {
    const stopped = meta({ status: "stopped", raw_packets: 100, events: 20 });
    expect(deriveSessionPhase({ meta: stopped, status: status({ state: "closed" }) })).toBe(
      "analyzable",
    );
    expect(
      deriveSessionPhase({ meta: meta({ status: "stopped" }), status: status({ state: "closed" }) }),
    ).toBe("empty");
  });

  it("停了但只有包没有事件也算可分析（至少能看原始包）", () => {
    const stopped = meta({ status: "stopped", raw_packets: 50, events: 0 });
    expect(deriveSessionPhase({ meta: stopped, status: status({ state: "closed" }) })).toBe(
      "analyzable",
    );
  });

  it("元数据 running 但实时态 closed 判定为中断（pipeline 重启后的 stale）", () => {
    expect(deriveSessionPhase({ meta: meta(), status: status({ state: "closed" }) })).toBe(
      "interrupted",
    );
  });

  it("停了的会话即使实时态缺失也不会误判为中断", () => {
    expect(deriveSessionPhase({ meta: meta({ status: "stopped" }), status: null })).toBe("empty");
  });

  it("非 agent 源零流量走 awaiting_traffic，不显示 Agent 相关文案", () => {
    const local = meta({ source: "nic" });
    expect(deriveSessionPhase({ meta: local, status: status({ source_name: "pcap-live" }) })).toBe(
      "awaiting_traffic",
    );
  });
});

describe("describeSessionPhase", () => {
  it("agent_idle 给出「启动游戏 / 执行一次操作 / 确认端口」三步指引", () => {
    const view = describeSessionPhase({
      meta: meta(),
      status: status({ agent_connected: true }),
    });
    expect(view.phase).toBe("agent_idle");
    expect(view.title).toContain("探针已连接");
    const tips = view.guidance?.steps.join(" ") ?? "";
    expect(tips).toContain("启动游戏");
    expect(tips).toContain("操作");
    expect(tips).toContain("9250");
  });

  it("事实核对表把「已连接 ✓ / Packets: 0」平铺出来", () => {
    const view = describeSessionPhase({
      meta: meta(),
      status: status({ agent_connected: true }),
    });
    expect(view.facts[0]).toMatchObject({ ok: true, value: "✓" });
    expect(view.facts[1]).toMatchObject({ ok: false, value: "Packets: 0" });
  });

  it("awaiting_agent 的进度条停在「连接探针」，且该步为 active", () => {
    const view = describeSessionPhase({
      meta: meta(),
      status: status({ agent_connected: false }),
    });
    const step = (key: string) => view.steps.find((s) => s.key === key);
    expect(step("link")).toMatchObject({ label: "连接探针", state: "active" });
    expect(step("traffic")?.state).toBe("pending");
  });

  it("非 agent 源把第二步换成「连接数据源」", () => {
    const view = describeSessionPhase({
      meta: meta({ source: "nic" }),
      status: status({ source_name: "pcap-live" }),
    });
    expect(view.steps.find((s) => s.key === "link")?.label).toBe("连接数据源");
  });

  it("empty 是失败态并给出下次该怎么做", () => {
    const view = describeSessionPhase({
      meta: meta({ status: "stopped", source: "nic" }),
      status: status({ state: "closed" }),
    });
    expect(view.phase).toBe("empty");
    expect(view.tone).toBe("error");
    expect(view.guidance?.steps.length ?? 0).toBeGreaterThan(0);
  });

  it("有包无事件且未配插件时提示配置问题", () => {
    const view = describeSessionPhase({
      meta: meta({ plugin: "" }),
      status: status({ agent_connected: true, raw_count: 200 }),
    });
    expect(view.phase).toBe("capturing");
    expect(view.guidance?.title).toContain("没有配置解析插件");
  });

  it("decoding 阶段无解码错误时不给指引（别拿噪音打扰用户）", () => {
    const view = describeSessionPhase({
      meta: meta(),
      status: status({ agent_connected: true, raw_count: 200, event_count: 40 }),
    });
    expect(view.guidance).toBeUndefined();
  });
});

describe("isInterrupted", () => {
  it("拿不到实时态时保守判定为未中断", () => {
    expect(isInterrupted(meta(), null)).toBe(false);
    expect(isInterrupted(meta(), status({ state: "running" }))).toBe(false);
  });
});

describe("fmtAgo", () => {
  it("未收到过数据时返回空串", () => {
    expect(fmtAgo(0)).toBe("");
    expect(fmtAgo(undefined)).toBe("");
  });

  it("按秒/分/小时人话化", () => {
    const now = Math.floor(Date.now() / 1000);
    expect(fmtAgo(now - 3)).toBe("刚刚");
    expect(fmtAgo(now - 42)).toBe("42 秒前");
    expect(fmtAgo(now - 300)).toBe("5 分钟前");
    expect(fmtAgo(now - 7200)).toBe("2 小时前");
  });
});
