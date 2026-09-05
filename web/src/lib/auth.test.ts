import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  authHeaders,
  clearAuthError,
  getAuthError,
  getIdentity,
  getToken,
  notifyAuthError,
  setIdentity,
  setToken,
  subscribeIdentity,
  subscribeToken,
  wasRecentlyUnauthorized,
  withTokenParam,
} from "@/lib/auth";

// node 环境没有 localStorage，用内存 Map 桩掉（auth.ts 全部经 safe 包装访问）。
const store = new Map<string, string>();
beforeEach(() => {
  vi.stubGlobal("localStorage", {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, v),
    removeItem: (k: string) => void store.delete(k),
  });
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  store.clear();
  setToken(null);
  setIdentity(null);
  clearAuthError();
});

describe("token 存取", () => {
  it("setToken/getToken 往返并持久化到 localStorage", () => {
    setToken("gt_aaa");
    expect(getToken()).toBe("gt_aaa");
    expect(store.get("gt_auth_token")).toBe("gt_aaa");
  });

  it("空串视为清除", () => {
    setToken("gt_aaa");
    setToken("   ");
    expect(getToken()).toBeNull();
    expect(store.has("gt_auth_token")).toBe(false);
  });

  it("authHeaders：有 token 带 Bearer，无 token 不带头", () => {
    expect(authHeaders()).toEqual({});
    setToken("gt_aaa");
    expect(authHeaders()).toEqual({ Authorization: "Bearer gt_aaa" });
  });
});

describe("withTokenParam", () => {
  it("无 token 时原样返回", () => {
    expect(withTokenParam("/events/plugins")).toBe("/events/plugins");
  });

  it("有 token 时拼查询参数并编码", () => {
    setToken("gametrace aaa/1");
    expect(withTokenParam("/events/plugins")).toBe(
      "/events/plugins?token=gametrace%20aaa%2F1",
    );
    expect(withTokenParam("/events/plugins?a=1")).toBe(
      "/events/plugins?a=1&token=gametrace%20aaa%2F1",
    );
  });
});

describe("identity", () => {
  it("set/set null 往返", () => {
    setIdentity({ owner: "alice", isAdmin: true });
    expect(getIdentity()).toEqual({ owner: "alice", isAdmin: true });
    setIdentity(null);
    expect(getIdentity()).toBeNull();
  });
});

describe("authError", () => {
  it("notifyAuthError 置位、setToken 清除", () => {
    expect(getAuthError()).toBe(false);
    notifyAuthError();
    expect(getAuthError()).toBe(true);
    setToken("gt_new");
    expect(getAuthError()).toBe(false);
  });
});

describe("wasRecentlyUnauthorized", () => {
  it("窗口内无 401 信号为 false；notifyAuthError 后立即为 true；超出窗口后回到 false", () => {
    const t0 = Date.now(); // 真实时间（此刻还没装假时钟）
    vi.useFakeTimers();
    // 推过默认 10s 窗口：抹平本文件更早用例可能残留的 lastUnauthorizedAt。
    vi.setSystemTime(t0 + 10_001);
    expect(wasRecentlyUnauthorized()).toBe(false);
    notifyAuthError();
    expect(wasRecentlyUnauthorized()).toBe(true);
    vi.setSystemTime(t0 + 20_002); // 距最近一次 notify 已超出窗口
    expect(wasRecentlyUnauthorized()).toBe(false);
  });

  it("flag 已置位时重复 notifyAuthError 仍刷新时间戳（幂等早退之前记录）", () => {
    const t0 = Date.now();
    vi.useFakeTimers();
    vi.setSystemTime(t0 + 10_001);
    notifyAuthError(); // 首次：置位 flag + 记录时间戳
    vi.setSystemTime(t0 + 19_000); // 距首次 9s，仍在窗口内
    notifyAuthError(); // 重复：flag 已置位走早退，但时间戳必须被刷新
    vi.setSystemTime(t0 + 28_000); // 距首次 18s（远超窗口），距第二次仅 9s
    expect(wasRecentlyUnauthorized()).toBe(true);
    expect(getAuthError()).toBe(true); // 幂等：flag 不因重复 notify 变化
  });
});

describe("订阅与联动", () => {
  it("token：setToken 变化时通知；退订后不再通知；重复退订安全", () => {
    const listener = vi.fn();
    const unsub = subscribeToken(listener);
    setToken("gt_aaa");
    expect(listener).toHaveBeenCalledTimes(1);
    unsub();
    unsub(); // 重复退订不应抛错
    setToken("gt_bbb");
    expect(listener).toHaveBeenCalledTimes(1);
  });

  it("identity：内容相等的新对象不通知（防无限重渲染守卫），内容变化才通知", () => {
    const listener = vi.fn();
    const unsub = subscribeIdentity(listener);
    setIdentity({ owner: "alice", isAdmin: true });
    expect(listener).toHaveBeenCalledTimes(1);
    // 新对象但 owner+isAdmin 相同：useSyncExternalStore 下每次回显都造新对象，
    // 若不拦住会无限重渲染，故必须静默早退。
    setIdentity({ owner: "alice", isAdmin: true });
    expect(listener).toHaveBeenCalledTimes(1);
    setIdentity({ owner: "alice", isAdmin: false });
    expect(listener).toHaveBeenCalledTimes(2);
    unsub();
  });

  it("换 token 联动清除 identity，等下一次响应头重新回显", () => {
    setToken("gt_old");
    setIdentity({ owner: "alice", isAdmin: false });
    expect(getIdentity()).toEqual({ owner: "alice", isAdmin: false });
    setToken("gt_new");
    expect(getIdentity()).toBeNull();
  });

  it("非对称：token 已为 null 时 setToken(null) 早退，不联动清除 identity", () => {
    expect(getToken()).toBeNull(); // 前置：afterEach 已把 token 清回匿名
    setIdentity({ owner: "bob", isAdmin: false });
    setToken(null); // v === token → 早退，identity 保持原样
    expect(getIdentity()).toEqual({ owner: "bob", isAdmin: false });
  });
});
