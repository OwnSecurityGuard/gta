/** list_raw_packets 返回的单个原始包 */
export interface RawPacket {
  id: string;
  timestamp: string;
  src: string;
  dst: string;
  protocol: string;
  /** base64 编码的 payload */
  payload: string;
  payload_len: number;
  link_type: number;
}

/** list_raw_packets 完整响应 */
export interface ListRawPacketsResult {
  ok: boolean;
  count: number;
  packets: RawPacket[];
}
