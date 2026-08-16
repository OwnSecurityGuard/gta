/** query_capture_table 完整响应。rows 为原始列行（proto3 空 repeated 会序列化为 null，前端需归一化）。 */
export interface QueryCaptureTableResult {
  ok?: boolean;
  table: string;
  count: number;
  rows: Record<string, unknown>[];
}
