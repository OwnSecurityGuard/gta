package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// WriteEvidenceGraph 写入证据图节点和边到 store。
// 同一 session 重复写入时，先删除旧数据（幂等覆盖）。
// analysisRun 标识此次分析运行（可为空），用于复现原则。
func (s *SQLiteStore) WriteEvidenceGraph(ctx context.Context, sessionID string, analysisRun string, nodes []EvidenceNodeRow, edges []EvidenceEdgeRow) error {
	if len(nodes) == 0 && len(edges) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 幂等覆盖：先删除该 session 的旧证据图
	if _, err := tx.ExecContext(ctx, "DELETE FROM evidence_edges WHERE session_id=?", sessionID); err != nil {
		return fmt.Errorf("delete old evidence edges: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM evidence_nodes WHERE session_id=?", sessionID); err != nil {
		return fmt.Errorf("delete old evidence nodes: %w", err)
	}

	// 写节点
	if len(nodes) > 0 {
		nodeStmt, err := tx.PrepareContext(ctx, `
			INSERT INTO evidence_nodes(id, session_id, kind, flow_id, analysis_run, timestamp, labels, properties, semantic)
			VALUES(?,?,?,?,?,?,?,?,?)
		`)
		if err != nil {
			return err
		}
		defer nodeStmt.Close()

		for _, n := range nodes {
			var flowID sql.NullString
			if n.FlowID != "" {
				flowID = sql.NullString{String: n.FlowID, Valid: true}
			}
			var ar sql.NullString
			if n.AnalysisRun != "" {
				ar = sql.NullString{String: n.AnalysisRun, Valid: true}
			}
			if _, err := nodeStmt.ExecContext(ctx, n.ID, n.SessionID, n.Kind, flowID, ar, n.Timestamp, n.Labels, n.Properties, n.Semantic); err != nil {
				return fmt.Errorf("insert evidence node %s: %w", n.ID, err)
			}
		}
	}

	// 写边
	if len(edges) > 0 {
		edgeStmt, err := tx.PrepareContext(ctx, `
			INSERT INTO evidence_edges(id, session_id, source, target, type, confidence, reason, analysis_run, properties, strength, method, rule_id, evidence_ids)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		`)
		if err != nil {
			return err
		}
		defer edgeStmt.Close()

		for _, e := range edges {
			var ar sql.NullString
			if e.AnalysisRun != "" {
				ar = sql.NullString{String: e.AnalysisRun, Valid: true}
			}
			if _, err := edgeStmt.ExecContext(ctx, e.ID, e.SessionID, e.Source, e.Target, e.Type, e.Confidence, e.Reason, ar, e.Properties, e.Strength, e.Method, e.RuleID, e.EvidenceIDs); err != nil {
				return fmt.Errorf("insert evidence edge %s: %w", e.ID, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	slog.Debug("wrote evidence graph", "session", sessionID, "nodes", len(nodes), "edges", len(edges))
	return nil
}

// QueryEvidenceGraph 查询证据图节点和边，支持邻接子图扩展。
func (s *SQLiteStore) QueryEvidenceGraph(ctx context.Context, q EvidenceGraphQuery) (*EvidenceGraphResult, error) {
	nodes, err := s.queryEvidenceNodes(ctx, q)
	if err != nil {
		return nil, err
	}

	edges, err := s.queryEvidenceEdges(ctx, q, nodes)
	if err != nil {
		return nil, err
	}

	return &EvidenceGraphResult{Nodes: nodes, Edges: edges}, nil
}

// QueryEvidenceEdges 按方向查询证据图边，用于节点链式追踪。
func (s *SQLiteStore) QueryEvidenceEdges(ctx context.Context, q EvidenceEdgeQuery) ([]EvidenceEdgeRow, error) {
	query := "SELECT id, session_id, source, target, type, confidence, COALESCE(reason,''), COALESCE(properties,'{}'), COALESCE(strength,''), COALESCE(method,''), COALESCE(rule_id,''), COALESCE(evidence_ids,'[]') FROM evidence_edges WHERE 1=1"
	var args []any

	if q.SessionID != "" {
		query += " AND session_id=?"
		args = append(args, q.SessionID)
	}
	if q.Source != "" {
		query += " AND source=?"
		args = append(args, q.Source)
	}
	if q.Target != "" {
		query += " AND target=?"
		args = append(args, q.Target)
	}
	if q.EdgeType != "" {
		query += " AND type=?"
		args = append(args, q.EdgeType)
	}
	query += " ORDER BY confidence DESC"
	if q.Limit > 0 {
		query += " LIMIT ? OFFSET ?"
		args = append(args, q.Limit, q.Offset)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []EvidenceEdgeRow
	for rows.Next() {
		var e EvidenceEdgeRow
		if err := rows.Scan(&e.ID, &e.SessionID, &e.Source, &e.Target, &e.Type, &e.Confidence, &e.Reason, &e.Properties, &e.Strength, &e.Method, &e.RuleID, &e.EvidenceIDs); err != nil {
			return nil, err
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// QueryEvidenceNodesByIDs 按节点 ID 列表批量查询节点。
func (s *SQLiteStore) QueryEvidenceNodesByIDs(ctx context.Context, sessionID string, ids []string) ([]EvidenceNodeRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	query := "SELECT id, session_id, kind, flow_id, timestamp, COALESCE(labels,'{}'), COALESCE(properties,'{}'), COALESCE(semantic,'') FROM evidence_nodes WHERE id IN (" + placeholders(len(ids)) + ")"
	var args []any
	for _, id := range ids {
		args = append(args, id)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []EvidenceNodeRow
	for rows.Next() {
		var n EvidenceNodeRow
		var flowID sql.NullString
		if err := rows.Scan(&n.ID, &n.SessionID, &n.Kind, &flowID, &n.Timestamp, &n.Labels, &n.Properties, &n.Semantic); err != nil {
			return nil, err
		}
		if flowID.Valid {
			n.FlowID = flowID.String
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// QueryEventNodeID 查询事件 ID 对应的证据图节点 ID（即 "evt_" + eventID）。
func (s *SQLiteStore) QueryEventNodeID(ctx context.Context, sessionID string, sessionEventID string) (string, error) {
	// 证据图节点 ID 规则：event 节点 = "evt_" + event.Identity.ID
	// 先尝试直接查询。
	nodeID := "evt_" + sessionEventID
	var exists bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM evidence_nodes WHERE id=? AND session_id=?)", nodeID, sessionID).Scan(&exists)
	if err != nil {
		return "", err
	}
	if exists {
		return nodeID, nil
	}
	// 也可能用户传的就是 node ID
	err = s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM evidence_nodes WHERE id=? AND session_id=?)", sessionEventID, sessionID).Scan(&exists)
	if err != nil {
		return "", err
	}
	if exists {
		return sessionEventID, nil
	}
	return "", fmt.Errorf("event node not found in evidence graph: %s (session=%s)", sessionEventID, sessionID)
}

func (s *SQLiteStore) queryEvidenceNodes(ctx context.Context, q EvidenceGraphQuery) ([]EvidenceNodeRow, error) {
	query := "SELECT id, session_id, kind, flow_id, timestamp, COALESCE(labels,'{}'), COALESCE(properties,'{}'), COALESCE(semantic,'') FROM evidence_nodes WHERE 1=1"
	var args []any

	if q.SessionID != "" {
		query += " AND session_id=?"
		args = append(args, q.SessionID)
	}
	if q.NodeKind != "" {
		query += " AND kind=?"
		args = append(args, q.NodeKind)
	}
	if q.FlowID != "" {
		query += " AND flow_id=?"
		args = append(args, q.FlowID)
	}

	if q.RootNodeID != "" && q.MaxDepth > 0 {
		nodeIDs, err := s.expandNodeIDs(ctx, q.SessionID, q.RootNodeID, q.MaxDepth)
		if err != nil {
			return nil, err
		}
		if len(nodeIDs) == 0 {
			return nil, nil
		}
		query += " AND id IN (" + placeholders(len(nodeIDs)) + ")"
		for _, id := range nodeIDs {
			args = append(args, id)
		}
	}

	query += " ORDER BY timestamp ASC"

	if q.Limit > 0 {
		query += " LIMIT ? OFFSET ?"
		args = append(args, q.Limit, q.Offset)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []EvidenceNodeRow
	for rows.Next() {
		var n EvidenceNodeRow
		var flowID sql.NullString
		if err := rows.Scan(&n.ID, &n.SessionID, &n.Kind, &flowID, &n.Timestamp, &n.Labels, &n.Properties, &n.Semantic); err != nil {
			return nil, err
		}
		if flowID.Valid {
			n.FlowID = flowID.String
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func (s *SQLiteStore) queryEvidenceEdges(ctx context.Context, q EvidenceGraphQuery, nodes []EvidenceNodeRow) ([]EvidenceEdgeRow, error) {
	query := "SELECT id, session_id, source, target, type, confidence, COALESCE(reason,''), COALESCE(properties,'{}'), COALESCE(strength,''), COALESCE(method,''), COALESCE(rule_id,''), COALESCE(evidence_ids,'[]') FROM evidence_edges WHERE 1=1"
	var args []any

	if q.SessionID != "" {
		query += " AND session_id=?"
		args = append(args, q.SessionID)
	}
	if q.EdgeType != "" {
		query += " AND type=?"
		args = append(args, q.EdgeType)
	}
	if q.MinConfidence > 0 {
		query += " AND confidence >= ?"
		args = append(args, q.MinConfidence)
	}

	if q.RootNodeID != "" && q.MaxDepth > 0 && len(nodes) > 0 {
		nodeIDSet := make(map[string]struct{}, len(nodes))
		for _, n := range nodes {
			nodeIDSet[n.ID] = struct{}{}
		}
		nodeIDs := make([]string, 0, len(nodeIDSet))
		for id := range nodeIDSet {
			nodeIDs = append(nodeIDs, id)
		}
		query += " AND source IN (" + placeholders(len(nodeIDs)) + ") AND target IN (" + placeholders(len(nodeIDs)) + ")"
		for _, id := range nodeIDs {
			args = append(args, id)
		}
		for _, id := range nodeIDs {
			args = append(args, id)
		}
	}

	if q.Limit > 0 {
		query += " LIMIT ? OFFSET ?"
		args = append(args, q.Limit, q.Offset)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []EvidenceEdgeRow
	for rows.Next() {
		var e EvidenceEdgeRow
		if err := rows.Scan(&e.ID, &e.SessionID, &e.Source, &e.Target, &e.Type, &e.Confidence, &e.Reason, &e.Properties, &e.Strength, &e.Method, &e.RuleID, &e.EvidenceIDs); err != nil {
			return nil, err
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// expandNodeIDs 从 rootNodeID 出发 BFS 收集可达节点 ID。
func (s *SQLiteStore) expandNodeIDs(ctx context.Context, sessionID, rootNodeID string, maxDepth int) ([]string, error) {
	ids := map[string]struct{}{rootNodeID: {}}
	frontier := []string{rootNodeID}

	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, fid := range frontier {
			rows, err := s.db.QueryContext(ctx,
				"SELECT source, target FROM evidence_edges WHERE session_id=? AND (source=? OR target=?)",
				sessionID, fid, fid)
			if err != nil {
				return nil, err
			}
			type pair struct{ source, target string }
			var pairs []pair
			for rows.Next() {
				var src, tgt string
				if err := rows.Scan(&src, &tgt); err != nil {
					rows.Close()
					return nil, err
				}
				pairs = append(pairs, pair{src, tgt})
			}
			rows.Close()
			for _, p := range pairs {
				for _, candidate := range []string{p.source, p.target} {
					if _, ok := ids[candidate]; !ok {
						ids[candidate] = struct{}{}
						next = append(next, candidate)
					}
				}
			}
		}
		frontier = next
	}

	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	return result, nil
}

// placeholders 返回 n 个 "?" 的逗号分隔字符串。
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, 0, n*2-1)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}
