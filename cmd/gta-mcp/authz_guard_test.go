package main

// authz_guard_test.go — AST 护栏（2026-09-05 方案 §3.3）。
//
// AuthZ 层防不住"新 handler 忘记鉴权"，本测试把"靠人记得"变成"CI 报错"：
//   1. 从 AST 提取 registerTools 里所有 s.AddTool(mcp.NewTool("<name>", ...), capture.<handler>)；
//   2. 工具名命中敏感模式（session / project / lease / access_code）的 handler，
//      函数体内必须出现受认可的鉴权入口调用（证据集见 authzEvidence）；
//   3. 明确豁免清单中的工具必须逐条给出理由 —— 新增豁免是显式的代码评审决策。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"testing"
)

// authzEvidence 是受认可的鉴权入口标识符：出现即视为已做权限校验。
// 它们本身都收敛到 pkg/authz 的判定（Can / 六步收口 / 项目 Action 加载）。
var authzEvidence = map[string]bool{
	"Can":                        true, // 直接调用 authz.Authorizer.Can
	"authorizeSession":           true, // 会话读取鉴权（authz.go）
	"projectForAction":           true, // 项目 Action 加载 + 鉴权（authz.go）
	"moveSessionToProject":       true, // 会话移动六步收口（project.go）
	"handleMoveSessionToProject": true, // 工具别名转发（set_session_project → move）
	"getDBPath":                  true, // 内部含 authorizeSession（main.go）
	"openReader":                 true, // 内部经 getDBPath 鉴权
	"visibleSessionFilter":       true, // 列表可见性 = owner ∪ 可见项目
	"AccessCodeActionAllowed":    true, // 启动码动作（pkg/authz）
}

// authzExemptions 是命敏感模式但确认无需 handler 级鉴权的工具，必须逐条给理由。
// 新增条目 = 一次显式的评审决策；删除条目前先给 handler 补上鉴权。
var authzExemptions = map[string]string{
	// 公开只读目录 / 自身范围列表：无跨用户资源访问。
	"list_projects":        "仅返回调用者可见项目（ListVisible 自带过滤）",
	"create_project":       "创建动作无既有资源可鉴权，Owner=创建者",
	"list_live_sessions":   "pipeline 侧 owner 作用域过滤（gRPC 透传 Owner/AllOwners）",
	"list_all_sessions":    "visibleSessionFilter 已含 owner ∪ 可见项目（证据集亦覆盖）",
	"list_access_codes":    "仅返回调用者自身 / admin 全量的启动码",
	"get_proxy_lease":      "租约归属由 pipeline 侧 owner 校验（lease↔project 绑定未落地，P2）",
	"get_session_timeline": "经 openReader → getDBPath → authorizeSession（证据集覆盖）",
	"list_proxy_leases":    "pipeline 侧 owner 作用域列表（透传 Owner/AllOwners）",
	"create_proxy_lease":   "租约归属由 pipeline 记录为调用者（透传 Owner/AllOwners）",
	"release_proxy_lease":  "pipeline 侧 owner 校验：非归属者释放被拒绝",
	"start_lease_capture":  "pipeline 侧 owner 校验（lease_id 归属）",
	"stop_lease_capture":   "pipeline 侧 owner 校验（lease_id 归属）",

	// 插件工具族：插件命名空间由 pipeline 按 owner 隔离（owner/name），
	// plugin↔project 绑定落地（方案 D6/P2）后再接 ActionPlugin*。
	"list_plugins":            "公开插件目录",
	"get_plugin_contract":     "公开契约文档",
	"get_plugin_dev_guide":    "公开开发指南",
	"create_plugin":           "pipeline 侧 owner 命名空间",
	"build_plugin":            "pipeline 侧 owner 命名空间",
	"activate_plugin":         "pipeline 侧 owner 命名空间",
	"deactivate_plugin":       "pipeline 侧 owner 命名空间",
	"status_plugin":           "pipeline 侧 owner 命名空间",
	"explain_plugin":          "公开解释器",
	"list_registered_plugins": "pipeline 侧 owner 作用域列表",
	"get_plugin_manifest":     "公开 manifest 查询",
	"deregister_plugin":       "pipeline 侧 owner 校验（注销他人插件被 pipeline 拒绝）",
	"test_plugin":             "pipeline 侧 owner 命名空间",
	"verify_plugin":           "pipeline 侧 owner 命名空间",
	"sample_bytes_plugin":     "pipeline 侧 owner 校验 + plugin_debug_access 审计留痕",
}

// TestSensitiveHandlersHaveAuthz 校验所有注册工具中命中敏感模式的 handler 都有鉴权证据。
func TestSensitiveHandlersHaveAuthz(t *testing.T) {
	handlers, err := registeredToolHandlers(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(handlers) < 30 {
		t.Fatalf("parsed only %d tool registrations; AST extraction is broken", len(handlers))
	}
	sensitive := regexp.MustCompile(`session|project|lease|access_code`)
	bodyHasEvidence := func(fn *ast.FuncDecl) bool {
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch e := n.(type) {
			case *ast.SelectorExpr:
				if authzEvidence[e.Sel.Name] {
					found = true
					return false
				}
			case *ast.Ident:
				if authzEvidence[e.Name] {
					found = true
					return false
				}
			}
			return !found
		})
		return found
	}

	// 收集包内全部 mcpCapture 方法声明，供证据检查。
	funcs := map[string]*ast.FuncDecl{}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !fi.IsDir() && filepath.Ext(fi.Name()) == ".go" && !regexp.MustCompile(`_test\.go$`).MatchString(fi.Name())
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil && len(fd.Recv.List) > 0 {
					if recv, ok := fd.Recv.List[0].Type.(*ast.StarExpr); ok {
						if id, ok := recv.X.(*ast.Ident); ok && id.Name == "mcpCapture" {
							funcs[fd.Name.Name] = fd
						}
					}
				}
			}
		}
	}

	for tool, handler := range handlers {
		if !sensitive.MatchString(tool) {
			continue
		}
		if _, exempt := authzExemptions[tool]; exempt {
			continue
		}
		fn, ok := funcs[handler]
		if !ok {
			t.Errorf("tool %q references handler %q which is not a mcpCapture method in this package", tool, handler)
			continue
		}
		if fn.Body == nil || !bodyHasEvidence(fn) {
			t.Errorf("handler %q (tool %q) 敏感但缺少 authz 鉴权调用；"+
				"请接入 m.authz.Can / authorizeSession / projectForAction，或在 authzExemptions 中给出豁免理由",
				handler, tool)
		}
	}
}

// registeredToolHandlers 从 AST 提取 工具名 → handler 方法名 的映射。
func registeredToolHandlers(t *testing.T) (map[string]string, error) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !fi.IsDir() && filepath.Ext(fi.Name()) == ".go" && !regexp.MustCompile(`_test\.go$`).MatchString(fi.Name())
	}, 0)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "AddTool" || len(call.Args) < 2 {
					return true
				}
				// Args[0]: mcp.NewTool("<name>", ...)
				newTool, ok := call.Args[0].(*ast.CallExpr)
				if !ok || len(newTool.Args) == 0 {
					return true
				}
				lit, ok := newTool.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				name := lit.Value[1 : len(lit.Value)-1]
				// Args[1]: capture.<handler>
				h, ok := call.Args[1].(*ast.SelectorExpr)
				if !ok {
					return true
				}
				out[name] = h.Sel.Name
				return true
			})
		}
	}
	return out, nil
}
