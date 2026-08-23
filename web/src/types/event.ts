/** list_decoded_data 返回的单个事件 */
export interface DecodedEvent {
  id: string;
  timestamp: string;
  session_id: string;
  protocol: string;
  raw_len: number;
  data: Record<string, unknown>;
  /** 代理抓包特有：捕获上下文（Captured By / Connection / Stream / Source）。 */
  capture?: import("./connection").CaptureContext;
}

/** list_decoded_data 完整响应 */
export interface ListDecodedDataResult {
  ok: boolean;
  total_matched: number;
  count: number;
  events: DecodedEvent[];
}

/** get_capture_schema 返回的列信息 */
export interface SchemaColumn {
  name: string;
  type: string;
  description: string;
}

/** get_capture_schema 返回的数据源 */
export interface SchemaSource {
  name: string;
  description: string;
  columns: SchemaColumn[];
}

/** get_capture_schema 返回的规则 */
export interface SchemaRule {
  name: string;
  filter: string;
  type: string;
  window: string;
  group_by: string[];
  value: string;
  output: string;
}

/** get_capture_schema 完整响应 */
export interface CaptureSchemaResult {
  ok: boolean;
  sources: SchemaSource[];
  query_fields: SchemaColumn[];
  rules: SchemaRule[];
  examples?: {
    aggregate_query?: string[];
    list_decoded_data_filter?: string[];
  };
}
