/** aggregate_query 返回的单个预计算指标 */
export interface AggregateMetric {
  name: string;
  window: string;
  group: Record<string, string>;
  value: number;
}

/** manifest 声明的可聚合字段（供编写 rules.yaml 对齐） */
export interface AggregatableField {
  schema: string;
  field: string;
  alias?: string;
}

/** aggregate_query 完整响应 */
export interface AggregateQueryResult {
  ok: boolean;
  count: number;
  metrics: AggregateMetric[];
  aggregatable_fields?: AggregatableField[];
}
