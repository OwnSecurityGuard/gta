/**
 * 「我的设备」接入状态：把「启动码 + 会话」两个已有数据源捏成用户可感知的设备状态。
 * 不引入 Device Service —— 一条启动码即一台待/已接入的电脑，认领后通过 session 派生运行态。
 */

/** 设备接入状态机。 */
export type DeviceState = "waiting" | "connected" | "capturing" | "stopped";

/** 由启动码（list_access_codes）与会话（list_all_sessions）派生的单台设备视图。 */
export interface DeviceView {
  /** 启动码（设备接入凭证，形如 GT-XXXX-XXXX） */
  code: string;
  /** 目标平台，如 windows/amd64 */
  platform?: string;
  /** 绑定的项目 ID */
  projectId?: string;
  /** 抓包端口 */
  port?: number;
  /** 解码插件 */
  plugin?: string;
  /** 当前接入状态 */
  state: DeviceState;
  /** 认领后建立的服务端会话 */
  sessionId?: string;
  /** 已抓包数 */
  packets: number;
  /** 已解码事件数 */
  events: number;
  /** 解码错误数 */
  decodeErrors: number;
  /** 最近活动时间（ISO） */
  lastSeen?: string;
  /** 是否已被认领（一次性启动码） */
  claimed: boolean;
  /** 创建时间（ISO） */
  createdAt: string;
}
