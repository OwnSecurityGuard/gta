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
