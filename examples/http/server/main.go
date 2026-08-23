package main

import (
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"sync/atomic"
)

// 消息类型常量：与协议语义配置（message.definitions）保持一致。
// 响应覆盖 response role / push rule / error 语义，并通过回显 seq 完成 request/response 关联。
const (
	cmdLoginRequest  = 1001 // 客户端请求（request）
	cmdLoginResponse = 1002 // 正常响应（response）
	cmdPlayerNotify  = 2001 // 服务端推送（push）
)

// header/body 信封结构：与客户端保持一致，供协议语义解析器提取语义。
type header struct {
	Cmd int `json:"cmd"` // message id
}

type body struct {
	Seq       int `json:"seq"`                  // seq correlation（回显请求的 seq 以配对）
	ErrorCode int `json:"error_code,omitempty"` // error 语义：0 成功，非 0 失败
}

type envelope struct {
	Header header `json:"header"`
	Body   body   `json:"body"`
}

func main() {
	addr := flag.String("addr", ":8984", "listen address")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	var count atomic.Int64
	mux := http.NewServeMux()

	handle := func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		req, parseErr := parseEnvelope(r)
		if parseErr != nil {
			slog.Warn("invalid request body", "n", n, "path", r.URL.Path, "error", parseErr)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(envelope{
				Header: header{Cmd: cmdLoginResponse},
				Body:   body{ErrorCode: 2},
			})
			return
		}
		slog.Info("received request", "n", n, "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr, "body", req)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(randomResponse(req))
	}
	mux.HandleFunc("/ping", handle)
	mux.HandleFunc("/echo", handle)

	slog.Info("http server listening", "addr", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}

// parseEnvelope 解析客户端发送的 header/body 信封。
func parseEnvelope(r *http.Request) (envelope, error) {
	var ev envelope
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return ev, err
	}
	if len(b) == 0 {
		return ev, io.ErrUnexpectedEOF
	}
	if err := json.Unmarshal(b, &ev); err != nil {
		return ev, err
	}
	return ev, nil
}

// randomResponse 随机生成响应，覆盖各类语义格式：
//   - cmd=1002 + 回显 seq：response role + seq correlation（与请求配对）
//   - cmd=2001 + seq=0：push rule（服务端主动推送）
//   - cmd=1002 + error_code=1：error 语义
func randomResponse(req envelope) envelope {
	switch rand.IntN(4) {
	case 0, 1: // 正常响应：回显 seq 完成 request/response 关联
		return envelope{Header: header{Cmd: cmdLoginResponse}, Body: body{Seq: req.Body.Seq}}
	case 2: // 服务端推送
		return envelope{Header: header{Cmd: cmdPlayerNotify}, Body: body{Seq: 0}}
	default: // 错误响应
		return envelope{Header: header{Cmd: cmdLoginResponse}, Body: body{Seq: req.Body.Seq, ErrorCode: 1}}
	}
}
