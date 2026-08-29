package plugindev

// SDKVersion 是脚手架固定引用的 gta-plugin-sdk 版本。
//
// 这是「开发指南（SDK Agents.md）/ 脚手架 / SDK」三者同版本发布的单一事实来源：
//   - go.mod.tmpl 通过 {{.SDKVersion}} 渲染 require 版本；
//   - create_plugin 把它作为 sdk_version 字段返回给调用方；
//   - 升级 SDK 时，必须同步修改本常量、SDK 自身版本与 Agents.md 中引用的版本，三者保持一致。
//
// 当前值对应 SDK 仓库 tag v0.4.1（语义契约 P1–P2：schema/state）。
const SDKVersion = "v0.4.1"

// FramingAvailable 标记当前 SDKVersion 是否包含 framing 包。
//
// framing 提供 link_type 剥离（framing.ExtractL7）与 TCP 重组
// （framing.NewReassembler），是 pcap 类流量解码的推荐前置步骤。
// 若某个 SDK 版本删除了 framing 包，必须将此常量置为 false —— 脚手架会据此
// 不 import framing 并在生成代码中显式标注「framing 不可用」，避免引用缺失的导入。
//
// v0.4.1 已包含 framing 包。
const FramingAvailable = true
