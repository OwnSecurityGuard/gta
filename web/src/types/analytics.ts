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

/** analyze_protocol_patterns：流统计 */
export interface PatternFlow {
  flow_id: string;
  event_count: number;
  c2s: number;
  s2c: number;
}

/** analyze_protocol_patterns：消息类型分布 */
export interface PatternEventType {
  event_type: string;
  count: number;
}

/** analyze_protocol_patterns：相关性流 */
export interface CorrelatedFlow {
  flow_id: string;
  correlation_count: number;
}

/** analyze_protocol_patterns：状态变更主体分布 */
export interface StateChangeSubject {
  subject_type: string;
  change_count: number;
  distinct_subjects: number;
}

/** analyze_protocol_patterns：状态变更操作分布 */
export interface StateChangePattern {
  subject_type: string;
  op: string;
  path: string;
  count: number;
}

/** analyze_protocol_patterns：证据图节点结构 */
export interface EvidenceGraphNodeStat {
  kind: string;
  node_count: number;
}

/** analyze_protocol_patterns：证据图边结构 */
export interface EvidenceGraphEdgeStat {
  type: string;
  edge_count: number;
  avg_confidence: number;
}

/** analyze_protocol_patterns：方向分布 */
export interface DirectionDist {
  direction: string;
  count: number;
}

/** analyze_protocol_patterns 完整响应（各分区均为可选，取决于是否有数据） */
export interface AnalyzePatternsResult {
  ok: boolean;
  flows?: PatternFlow[];
  event_types?: PatternEventType[];
  correlated_flows?: CorrelatedFlow[];
  state_change_subjects?: StateChangeSubject[];
  state_change_patterns?: StateChangePattern[];
  evidence_graph_nodes?: EvidenceGraphNodeStat[];
  evidence_graph_edges?: EvidenceGraphEdgeStat[];
  direction_distribution?: DirectionDist[];
}
