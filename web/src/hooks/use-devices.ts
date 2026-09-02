import { useMemo } from "react";
import { useAccessCodes, useSessions } from "@/hooks/use-mcp";
import type { DeviceView } from "@/types/device";
import type { AccessCode } from "@/types/access-code";
import type { SessionInfo } from "@/types/session";

/**
 * 从启动码 + 会话派生「我的设备」列表（最新接入在前）。
 *
 * 状态推导规则（不引入 Device Service）：
 * - 未认领 → waiting（等待接入）
 * - 已认领 + 会话 running + packets>0 → capturing（正在抓包）
 * - 已认领 + 会话 running + packets==0 → connected（已连接，尚未收到流量）
 * - 已认领 + 会话已停止 → stopped
 * - 已认领但会话尚未出现在列表 → connected（连接已建立，会话同步中）
 */
function deriveDevices(codes: AccessCode[], sessions: SessionInfo[]): DeviceView[] {
  const bySession = new Map<string, SessionInfo>();
  for (const s of sessions) {
    if (s.session_id) bySession.set(s.session_id, s);
  }

  const sorted = [...codes].sort((a, b) => {
    const ta = a.created_at ? new Date(a.created_at).getTime() : 0;
    const tb = b.created_at ? new Date(b.created_at).getTime() : 0;
    return tb - ta;
  });

  return sorted.map((c) => {
    const session = c.session_id ? bySession.get(c.session_id) : undefined;
    const running = session?.status === "running";
    const stopped = !!session && !running;
    const packets = session?.raw_packets ?? 0;
    const events = session?.events ?? 0;
    const decodeErrors = session?.decode_errors ?? 0;

    let state: DeviceView["state"];
    if (!c.claimed) state = "waiting";
    else if (stopped) state = "stopped";
    else if (running && packets > 0) state = "capturing";
    else state = "connected";

    return {
      code: c.code,
      platform: c.platform || undefined,
      projectId: c.project_id,
      port: c.port ?? undefined,
      plugin: c.plugin || undefined,
      state,
      sessionId: c.session_id || session?.session_id,
      packets,
      events,
      decodeErrors,
      lastSeen: running ? session?.started_at : session?.stopped_at,
      claimed: !!c.claimed,
      createdAt: c.created_at,
    };
  });
}

export function useMyDevices(): DeviceView[] {
  const { data: codesData } = useAccessCodes();
  const { data: sessionsData } = useSessions();
  const codes = codesData?.codes ?? [];
  const sessions = sessionsData?.sessions ?? [];
  return useMemo(() => deriveDevices(codes, sessions), [codes, sessions]);
}
