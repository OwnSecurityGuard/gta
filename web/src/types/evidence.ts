/** query_evidence_graph 返回的节点 */
export interface EvidenceNode {
  id: string;
  session_id: string;
  kind: string;
  timestamp: string;
  flow_id?: string;
  /** v1 Semantic 投影（事件节点的语义字段），由后端确定性投影产出 */
  semantic?: Record<string, unknown>;
}

/** query_evidence_graph 返回的边 */
export interface EvidenceEdge {
  id: string;
  session_id: string;
  source: string;
  target: string;
  type: string;
  confidence: number;
  reason?: string;
  strength?: string;
  method?: string;
  rule_id?: string;
  evidence_ids?: string[];
}

/** query_evidence_graph 完整响应 */
export interface EvidenceGraphResult {
  ok: boolean;
  count: number;
  nodes: EvidenceNode[];
  edges: EvidenceEdge[];
}

/** trace_event_chain 的单向跳点（上游/下游） */
export interface TraceHop {
  node_id: string;
  depth: number;
  edge_type: string;
  confidence: number;
  reason?: string;
  strength?: string;
  method?: string;
  rule_id?: string;
  evidence_ids?: string[];
}

/** trace_event_chain 完整响应 */
export interface TraceEventChainResult {
  ok: boolean;
  start_node_id: string;
  nodes: EvidenceNode[];
  edges: EvidenceEdge[];
  upstream: TraceHop[];
  downstream: TraceHop[];
}

/** suggest_link_rules 的单条规则建议 */
export interface LinkRuleSuggestion {
  edge_type: string;
  source_type: string;
  target_type: string;
  occurrences: number;
  avg_confidence: number;
  rule_template: string;
  notes?: string[];
}

/** suggest_link_rules 完整响应 */
export interface SuggestLinkRulesResult {
  ok: boolean;
  session_id: string;
  suggestions: LinkRuleSuggestion[];
  total_edges: number;
  total_nodes: number;
}
