import { useSyncExternalStore } from "react";
import {
  getAuthError,
  getIdentity,
  getToken,
  subscribeAuthError,
  subscribeIdentity,
  subscribeToken,
  type Identity,
} from "@/lib/auth";

/** 当前访问令牌（token 变化时组件重渲染，SSE 由此触发重连）。 */
export function useAuthToken(): string | null {
  return useSyncExternalStore(subscribeToken, getToken);
}

/** 当前身份（来自后端 X-GT-Owner/X-GT-Admin 响应头回显；匿名模式为 null）。 */
export function useIdentity(): Identity | null {
  return useSyncExternalStore(subscribeIdentity, getIdentity);
}

/** 是否处于 401 待补令牌状态（App 层显示横幅）。 */
export function useAuthError(): boolean {
  return useSyncExternalStore(subscribeAuthError, getAuthError);
}
