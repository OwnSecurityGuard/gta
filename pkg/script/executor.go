package script

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ExecResult 脚本执行结果
type ExecResult struct {
	Stdout    string        `json:"stdout"`
	Stderr    string        `json:"stderr"`
	ExitCode  int           `json:"exit_code"`
	Duration  time.Duration `json:"duration"`
	StartTime time.Time     `json:"start_time"`
	EndTime   time.Time     `json:"end_time"`
}

// Executor 脚本执行器
type Executor struct {
	pythonPath string
	apiDir     string
	timeout    time.Duration
	workDir    string
	mcpURL     string
	outputDir  string
}

// NewExecutor 创建脚本执行器
// workDir 与 mcpURL 用于构造脚本运行时的 GTA_WORK_DIR / GTA_OUTPUT_DIR / GTA_MCP_URL
// 环境变量，避免调用方遗漏。
func NewExecutor(pythonPath, apiDir, workDir, mcpURL string, timeout time.Duration) *Executor {
	if workDir == "" {
		workDir = "."
	}
	if mcpURL == "" {
		mcpURL = "http://localhost:8090/mcp"
	}
	return &Executor{
		pythonPath: pythonPath,
		apiDir:     apiDir,
		timeout:    timeout,
		workDir:    workDir,
		mcpURL:     mcpURL,
		outputDir:  workDir,
	}
}

// Execute 执行脚本
func (e *Executor) Execute(ctx context.Context, scriptPath string, args map[string]string, env map[string]string) (*ExecResult, error) {
	// 验证脚本存在
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("script not found: %s", scriptPath)
	}

	// 创建带超时的 context
	if e.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}

	// 构建命令
	cmd := exec.CommandContext(ctx, e.pythonPath, scriptPath)

	// 设置环境变量：先继承系统环境，再添加 GTA 相关变量。
	// 调用方传入的 env 可覆盖默认值。
	cmd.Env = append(os.Environ(),
		"PYTHONPATH="+e.apiDir,
		"GTA_ARGS="+mustMarshalJSON(args),
		"GTA_WORK_DIR="+e.workDir,
		"GTA_MCP_URL="+e.mcpURL,
		"GTA_OUTPUT_DIR="+e.outputDir,
	)
	// 添加调用方传入的额外环境变量
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	// 捕获输出
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// 记录开始时间
	startTime := time.Now()

	// 执行命令
	err := cmd.Run()

	// 记录结束时间
	endTime := time.Now()
	duration := endTime.Sub(startTime)

	// 构建结果
	result := &ExecResult{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		StartTime: startTime,
		EndTime:   endTime,
		Duration:  duration,
	}

	// 判断退出码
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else if ctx.Err() == context.DeadlineExceeded {
			result.ExitCode = -1
			result.Stderr += "\n[TIMEOUT] Script execution timed out"
		} else {
			return nil, fmt.Errorf("execute script: %w", err)
		}
	} else {
		result.ExitCode = 0
	}

	return result, nil
}

// mustMarshalJSON 序列化为 JSON，失败时返回空对象
func mustMarshalJSON(v interface{}) string {
	if v == nil {
		return "{}"
	}
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// GetAPIModulePath 获取 API 模块路径
func (e *Executor) GetAPIModulePath() string {
	return filepath.Join(e.apiDir, "gta_api.py")
}
