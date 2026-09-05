package main

import "testing"

func TestParseFilterPushdown(t *testing.T) {
	cases := []struct {
		name   string
		filter string
		want   filterPushdown
	}{
		{"空表达式", "", filterPushdown{Pure: true}},
		{"空白", "   ", filterPushdown{Pure: true}},
		{"protocol 等值", `protocol == "tcp"`, filterPushdown{TypeEq: "tcp", Pure: true}},
		{"protocol 等值 反序", `"http" == protocol`, filterPushdown{TypeEq: "http", Pure: true}},
		{"protocol 不等", `protocol != "dns"`, filterPushdown{TypeNot: "dns", Pure: true}},
		{"and 关键字", `protocol == "tcp" and data.x == 1`, filterPushdown{TypeEq: "tcp"}},
		{"混合合取", `protocol == "tcp" && data.path contains "/api"`, filterPushdown{TypeEq: "tcp"}},
		{"多 protocol 合取不 Pure", `protocol == "a" && protocol == "b"`, filterPushdown{TypeEq: "a"}},
		{"纯 payload 过滤", `data.msg_name == "Login"`, filterPushdown{}},
		{"or 整体 residual", `protocol == "tcp" || protocol == "http"`, filterPushdown{}},
		{"not residual", `not (protocol == "tcp")`, filterPushdown{}},
		{"contains residual", `protocol contains "cp"`, filterPushdown{}},
		{"数值常量不识别", `protocol == 123`, filterPushdown{}},
		{"raw_len residual", `raw_len > 100`, filterPushdown{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseFilterPushdown(c.filter)
			if err != nil {
				t.Fatalf("parse %q: %v", c.filter, err)
			}
			if got != c.want {
				t.Fatalf("parse %q: got %+v want %+v", c.filter, got, c.want)
			}
		})
	}
}

func TestParseFilterPushdown_ParseError(t *testing.T) {
	if _, err := parseFilterPushdown(`protocol ==`); err == nil {
		t.Fatal("invalid expr should error")
	}
}
