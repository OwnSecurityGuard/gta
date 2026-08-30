package main

import (
	"strings"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
)

// filterPushdown 描述从 filter 表达式提取的可下推 SQL 谓词。
// 下推仅是优化（缩小候选集），不做正确性判定——除 Pure 路径外，
// 完整表达式仍在应用层对候选行求值，保证语义与旧行为完全一致。
type filterPushdown struct {
	// TypeEq / TypeNot 是 events.type 等值/不等下推条件（互斥，最多一个生效）。
	TypeEq  string
	TypeNot string
	// Pure 表示整个 filter 已被 SQL 完全表达（无 filter，或恰好是单个
	// protocol 比较），可走纯 SQL 分页：COUNT 取 total、跳过应用层求值。
	Pure bool
}

// parseFilterPushdown 解析 filter 表达式的顶层合取项，提取 protocol ==/!= "常量"
// 谓词作为 type 下推条件。
//
// 保守规则：
//   - 只拆分顶层 && / and 合取链；出现 || / not / 其他结构即存在 residual；
//   - 仅识别 protocol 与字符串常量的 == / !=（常量在任一侧均可）；
//   - 多合取项一律不 Pure（即使每项都可下推，如 protocol=="a" && protocol=="b"
//     是空集，必须交给应用层求值得到 0 结果）；
//   - 解析失败返回 error，调用方回退旧全量过滤路径。
func parseFilterPushdown(filterExpr string) (filterPushdown, error) {
	filterExpr = strings.TrimSpace(filterExpr)
	if filterExpr == "" {
		return filterPushdown{Pure: true}, nil
	}

	tree, err := parser.Parse(filterExpr)
	if err != nil {
		return filterPushdown{}, err
	}

	conjuncts := splitConjuncts(tree.Node)
	pd := filterPushdown{}
	pure := true
	for _, c := range conjuncts {
		eq, ne, ok := protocolComparison(c)
		if !ok {
			pure = false
			continue
		}
		// 首个 protocol 比较作为下推条件；后续 protocol 比较不再下推
		//（避免多条件互斥时错误收紧，语义由应用层完整求值保证）。
		if pd.TypeEq == "" && pd.TypeNot == "" {
			pd.TypeEq, pd.TypeNot = eq, ne
		}
	}
	pd.Pure = pure && len(conjuncts) <= 1
	return pd, nil
}

// splitConjuncts 展开顶层 && / and 合取链，返回待分类的合取项。
// 非 AND 结构（||、not、函数调用等）作为整体返回，交由 protocolComparison 判定。
func splitConjuncts(node ast.Node) []ast.Node {
	if b, ok := node.(*ast.BinaryNode); ok && (b.Operator == "&&" || b.Operator == "and") {
		return append(splitConjuncts(b.Left), splitConjuncts(b.Right)...)
	}
	return []ast.Node{node}
}

// protocolComparison 判断节点是否是 protocol 与字符串常量的 == / != 比较，
// 返回 (等值常量, 不等常量, 是否识别)。
func protocolComparison(node ast.Node) (eq, ne string, ok bool) {
	b, ok := node.(*ast.BinaryNode)
	if !ok || (b.Operator != "==" && b.Operator != "!=") {
		return "", "", false
	}

	const name = "protocol"
	leftIdent, leftIsIdent := b.Left.(*ast.IdentifierNode)
	rightIdent, rightIsIdent := b.Right.(*ast.IdentifierNode)
	leftStr, leftIsStr := b.Left.(*ast.StringNode)
	rightStr, rightIsStr := b.Right.(*ast.StringNode)

	switch {
	case leftIsIdent && leftIdent.Value == name && rightIsStr:
		return constantPair(b.Operator, rightStr.Value)
	case rightIsIdent && rightIdent.Value == name && leftIsStr:
		return constantPair(b.Operator, leftStr.Value)
	default:
		return "", "", false
	}
}

// constantPair 按比较算子返回 (eq, ne)。
func constantPair(op, value string) (eq, ne string, ok bool) {
	if op == "==" {
		return value, "", true
	}
	return "", value, true
}
