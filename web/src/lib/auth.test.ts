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
  vi.unstubAllGlobals();
  store.clear();
  setToken(null);
  setIdentity(null);
  clearAuthError();
});

describe("token 存取", () => {
  it("setToken/getToken 往返并持久化到 localStorage", () => {
    setToken("gta_aaa");
    expect(getToken()).toBe("gta_aaa");
    expect(store.get("gta_auth_token")).toBe("gta_aaa");
  });

  it("空串视为清除", () => {
    setToken("gta_aaa");
    setToken("   ");
    expect(getToken()).toBeNull();
    expect(store.has("gta_auth_token")).toBe(false);
  });

  it("authHeaders：有 token 带 Bearer，无 token 不带头", () => {
    expect(authHeaders()).toEqual({});
    setToken("gta_aaa");
    expect(authHeaders()).toEqual({ Authorization: "Bearer gta_aaa" });
  });
});

describe("withTokenParam", () => {
  it("无 token 时原样返回", () => {
    expect(withTokenParam("/events/plugins")).toBe("/events/plugins");
  });

  it("有 token 时拼查询参数并编码", () => {
    setToken("gta aaa/1");
    expect(withTokenParam("/events/plugins")).toBe(
      "/events/plugins?token=gta%20aaa%2F1",
    );
    expect(withTokenParam("/events/plugins?a=1")).toBe(
      "/events/plugins?a=1&token=gta%20aaa%2F1",
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
    setToken("gta_new");
    expect(getAuthError()).toBe(false);
  });
});

describe("订阅与联动", () => {
  it("token：setToken 变化时通知；退订后不再通知；重复退订安全", () => {
    const listener = vi.fn();
    const unsub = subscribeToken(listener);
    setToken("gta_aaa");
    expect(listener).toHaveBeenCalledTimes(1);
    unsub();
    unsub(); // 重复退订不应抛错
    setToken("gta_bbb");
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
    setToken("gta_old");
    setIdentity({ owner: "alice", isAdmin: false });
    expect(getIdentity()).toEqual({ owner: "alice", isAdmin: false });
    setToken("gta_new");
    expect(getIdentity()).toBeNull();
  });

  it("非对称：token 已为 null 时 setToken(null) 早退，不联动清除 identity", () => {
    expect(getToken()).toBeNull(); // 前置：afterEach 已把 token 清回匿名
    setIdentity({ owner: "bob", isAdmin: false });
    setToken(null); // v === token → 早退，identity 保持原样
    expect(getIdentity()).toEqual({ owner: "bob", isAdmin: false });
  });
});
