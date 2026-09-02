package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"time"
)

// 消息类型常量：与协议语义配置（message.definitions）保持一致，
// 用于覆盖 message id / request-response role / seq correlation / push rule / error 五类格式。
const (
	cmdLoginRequest = 1001 // 请求消息（role=request）
	cmdPlayerNotify = 2001 // 推送消息（role=push，命中 push rule: header.cmd==2001 或 body.seq==0）
)

// header/body 信封结构：协议语义解析器按 header.cmd / body.seq / body.error_code 提取语义。
type header struct {
	Cmd int `json:"cmd"` // message id
}

type body struct {
	Seq       int `json:"seq"`                  // seq correlation（请求/响应配对键）
	ErrorCode int `json:"error_code,omitempty"` // error 语义：0 成功，非 0 失败
}

type envelope struct {
	Header header `json:"header"`
	Body   body   `json:"body"`
}

func main() {
	addr := flag.String("addr", "http://127.0.0.1:8984", "server base url")
	interval := flag.Duration("interval", 5*time.Second, "request interval")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	client := &http.Client{Timeout: 5 * time.Second}
	tick := time.NewTicker(*interval)
	defer tick.Stop()

	for i := 1; ; i++ {
		ev := randomEnvelope()
		path := "/ping"
		if rand.IntN(2) == 0 {
			path = "/echo"
		}
		payload, _ := json.Marshal(ev)
		req, err := http.NewRequest(http.MethodPost, *addr+path, bytes.NewReader(payload))
		if err != nil {
			slog.Error("build request", "error", err)
			<-tick.C
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		slog.Info("send", "n", i, "path", path, "body", string(payload))

		resp, err := client.Do(req)
		if err != nil {
			slog.Error("request failed", "n", i, "error", err)
		} else {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			slog.Info("receive", "n", i, "status", resp.StatusCode, "body", string(bytes.TrimSpace(b)))
		}

		<-tick.C
	}
}

// randomEnvelope 随机生成一条请求消息，覆盖各类语义格式：
//   - cmd=1001 + 非零 seq：request role + message id + seq correlation
//   - cmd=2001 或 seq=0：push rule
//   - error_code=1：error 语义
func randomEnvelope() envelope {
	switch rand.IntN(3) {
	case 0: // 正常请求
		return envelope{Header: header{Cmd: cmdLoginRequest}, Body: body{Seq: randSeq()}}
	case 1: // 推送风格请求（命中 push rule）
		if rand.IntN(2) == 0 {
			return envelope{Header: header{Cmd: cmdPlayerNotify}, Body: body{Seq: 0}}
		}
		return envelope{Header: header{Cmd: cmdLoginRequest}, Body: body{Seq: 0}}
	default: // 错误请求
		return envelope{Header: header{Cmd: cmdLoginRequest}, Body: body{Seq: randSeq(), ErrorCode: 1}}
	}
}

// randSeq 生成 1000~9999 的随机 seq。
func randSeq() int { return rand.IntN(9000) + 1000 }
