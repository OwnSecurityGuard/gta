package main

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
)

// capabilityCatalog 是 get_capabilities 的静态目录：按工作流分组列出全部
// 工具与推荐调用链。给 AI Agent 一个自描述入口，避免靠 README 或试错来
// 理解工具之间的关系。新增工具时同步维护本目录（与 main.go 的 AddTool 对齐）。
type toolGroup struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tools       []string `json:"tools"`
}

type capabilityDoc struct {
	Server      string      `json:"server"`
	Groups      []toolGroup `json:"groups"`
	TypicalFlow []string    `json:"typical_flow"`
	Notes       []string    `json:"notes"`
}

func buildCapabilityCatalog() capabilityDoc {
	return capabilityDoc{
		Server: "game-debug-automation",
		Groups: []toolGroup{
			{
				Name:        "capture",
				Description: "抓包与会话生命周期",
				Tools: []string{
					"start_capture", "stop_capture", "get_session_status",
					"list_interfaces", "list_live_sessions", "set_session_plugin",
					"list_all_sessions", "delete_session",
				},
			},
			{
				Name:        "query",
				Description: "解码事件 / 状态 / 聚合 / 执行链查询",
				Tools: []string{
					"list_decoded_data", "list_state_changes",
					"aggregate_query", "get_capture_schema",
					"trace_protocol_flow", "query_capture_table",
				},
			},
			{
				Name:        "behavior",
				Description: "操作窗口标记（不启动抓包）",
				Tools:       []string{"begin_capture_run", "end_capture_run", "get_run_status"},
			},
			{
				Name:        "plugin-dev",
				Description: "Developer Plane：脚手架 / 编译 / 拉起 / 归因",
				Tools: []string{
					"create_plugin", "build_plugin", "activate_plugin", "deactivate_plugin",
					"status_plugin", "explain_plugin",
				},
			},
			{
				Name:        "plugin-verify",
				Description: "Runtime Plane：契约校验与受限取样",
				Tools:       []string{"test_plugin", "verify_plugin", "sample_bytes_plugin"},
			},
			{
				Name:        "plugin-runtime",
				Description: "注册表观测与 manifest",
				Tools: []string{
					"list_plugins", "list_registered_plugins",
					"get_plugin_manifest", "deregister_plugin", "get_registry_addr",
				},
			},
			{
				Name:        "plugin-knowledge",
				Description: "契约 SSOT 与开发指南（写插件前先读）",
				Tools:       []string{"get_plugin_contract", "get_plugin_dev_guide", "get_capabilities"},
			},
			{
				Name:        "raw-debug",
				Description: "原始包调试，需服务端 -enable-raw-debug，默认不注册",
				Tools:       []string{"list_raw_packets", "decode_raw_packets"},
			},
		},
		TypicalFlow: []string{
			"接入新协议: get_plugin_dev_guide -> create_plugin -> build_plugin -> start_capture(plugin=...) -> activate_plugin -> verify_plugin -> get_capture_schema -> list_decoded_data",
			"分析已有会话: list_all_sessions -> list_decoded_data / aggregate_query -> trace_protocol_flow",
			"定位解码为空: status_plugin -> get_registry_addr -> sample_bytes_plugin -> explain_plugin",
		},
		Notes: []string{
			"begin_capture_run 的 plugin_name/device/filter/port 是描述性提示，不会自动启动抓包；启动抓包必须显式调用 start_capture",
			"query_capture_table 是内部投影/审计表的只读逃生口（allowlist 含 event_index / plugin_debug_access）",
		},
	}
}

func (m *mcpCapture) handleGetCapabilities(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return successResult(buildCapabilityCatalog()), nil
}
