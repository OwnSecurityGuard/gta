/** test_plugin 返回的单个采样解码事件（插件解出来的相关数据，不含原始字节） */
export interface TestEventLite {
  id: string;
  timestamp_unix: number;
  type: string;
  schema_id: string;
  /** 拍平后的关键 data.* 字段 JSON（可能截断） */
  data_json: string;
}

/** test_plugin 返回的单个解码失败样例（仅含定位信息，不含原始字节） */
export interface TestErrorLite {
  raw_packet_id: string;
  src: string;
  dst: string;
  error: string;
}

/** test_plugin 完整响应 */
export interface TestPluginResult {
  ok?: boolean;
  status: string;
  session_id: string;
  plugin: string;
  total_raw: number;
  decoded: number;
  decode_errors: number;
  type_histogram: Record<string, number>;
  sample_events: TestEventLite[];
  error_samples: TestErrorLite[];
}

/** useTestPlugin 入参 */
export interface TestPluginVars {
  sessionId: string;
  plugin: string;
  protocol?: string;
  src?: string;
  dst?: string;
  limit?: number;
  sampleLimit?: number;
}
