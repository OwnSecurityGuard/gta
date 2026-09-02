import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

/** shadcn-ui 标准的 className 合并工具 */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

/**
 * 递归地把"值是 JSON 字符串"的字段还原成对象/数组。
 * 用于去除存储为字符串的 JSON body 在展示时出现的转义符（如 HTTP body 是 JSON 文本）。
 * 仅对以 { 或 [ 开头的字符串尝试解析，避免误伤普通文本或裸标量（如 "pong 1297"）。
 * 已为对象/数组的值原样返回，因此对结构化数据（新捕获）无副作用。
 */
export function unpackJsonStrings<T>(value: T): T {
  if (Array.isArray(value)) {
    return value.map((v) => unpackJsonStrings(v)) as unknown as T;
  }
  if (value && typeof value === "object") {
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
      out[k] = unpackJsonStrings(v);
    }
    return out as unknown as T;
  }
  if (typeof value === "string") {
    const trimmed = value.trimStart();
    if (trimmed.length > 1 && (trimmed.startsWith("{") || trimmed.startsWith("["))) {
      try {
        const parsed = JSON.parse(value);
        if (parsed !== null && typeof parsed === "object") return parsed as unknown as T;
      } catch {
        // 不是合法 JSON，保留原字符串
      }
    }
  }
  return value;
}

