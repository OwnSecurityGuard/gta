// Package internalipc errors.go 定义 typed error code 用于 gRPC 错误传播（spec §11）。
//
// handler 返回这些错误时，gRPC 自动将其转为对应 status code；
// 客户端可通过 status.Code(err) 获取语义化错误码，或解析 message 前缀获取 errCode。
package internalipc

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 错误码常量，作为 gRPC status message 的前缀。
const (
	CodeNoActiveCapture = "NO_ACTIVE_CAPTURE"
	CodeAlreadyActive   = "ALREADY_ACTIVE"
	CodeSourceEmpty     = "SOURCE_CONFIG_EMPTY"
	CodeAlreadyStarted  = "ALREADY_STARTED"
)

// 预定义 gRPC status error。handler 直接返回这些变量。
var (
	ErrNoActiveCapture = status.Error(codes.FailedPrecondition, CodeNoActiveCapture+": no active capture session")
	ErrAlreadyActive   = status.Error(codes.AlreadyExists, CodeAlreadyActive+": a capture session is already active; stop it first")
	ErrSourceEmpty     = status.Error(codes.InvalidArgument, CodeSourceEmpty+": start_capture: source config is empty")
	// ErrAlreadyStarted 表示 captureTask 已启动（CAS Created→Running 失败）。
	// 正常流程不应触发——pipelineService 每次创建新 task。
	ErrAlreadyStarted = status.Error(codes.Internal, CodeAlreadyStarted+": capture task already started")
)
