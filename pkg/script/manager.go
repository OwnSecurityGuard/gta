package script

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ScriptInfo 脚本元信息
type ScriptInfo struct {
	Name      string    `json:"name"`
	Scope     string    `json:"scope"` // "global" or "session"
	SessionID string    `json:"session_id,omitempty"`
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mod_time"`
}

// Manager 脚本管理器
type Manager struct {
	baseDir string
}

// NewManager 创建脚本管理器
func NewManager(baseDir string) (*Manager, error) {
	// 创建全局脚本目录
	globalDir := filepath.Join(baseDir, "scripts", "global")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		return nil, fmt.Errorf("create global script dir: %w", err)
	}

	return &Manager{baseDir: baseDir}, nil
}

// SaveScript 保存脚本
func (m *Manager) SaveScript(name, code, scope, sessionID string) (*ScriptInfo, error) {
	// 验证脚本名
	if err := validateScriptName(name); err != nil {
		return nil, err
	}

	// 确定脚本路径
	scriptPath, err := m.getScriptPath(name, scope, sessionID)
	if err != nil {
		return nil, err
	}

	// 确保目录存在
	dir := filepath.Dir(scriptPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create script dir: %w", err)
	}

	// 写入文件
	if err := os.WriteFile(scriptPath, []byte(code), 0644); err != nil {
		return nil, fmt.Errorf("write script: %w", err)
	}

	// 获取文件信息
	info, err := os.Stat(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("stat script: %w", err)
	}

	return &ScriptInfo{
		Name:      name,
		Scope:     scope,
		SessionID: sessionID,
		Path:      scriptPath,
		Size:      info.Size(),
		ModTime:   info.ModTime(),
	}, nil
}

// ListScripts 列出脚本
func (m *Manager) ListScripts(scope, sessionID string) ([]ScriptInfo, error) {
	var scripts []ScriptInfo

	if scope == "global" {
		// 列出全局脚本
		globalDir := filepath.Join(m.baseDir, "scripts", "global")
		files, err := os.ReadDir(globalDir)
		if err != nil {
			if os.IsNotExist(err) {
				return scripts, nil
			}
			return nil, fmt.Errorf("read global dir: %w", err)
		}

		for _, f := range files {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".py") {
				info, err := f.Info()
				if err != nil {
					continue
				}
				scripts = append(scripts, ScriptInfo{
					Name:    strings.TrimSuffix(f.Name(), ".py"),
					Scope:   "global",
					Path:    filepath.Join(globalDir, f.Name()),
					Size:    info.Size(),
					ModTime: info.ModTime(),
				})
			}
		}
	} else if scope == "session" {
		if sessionID == "" {
			return nil, fmt.Errorf("session_id required for session scope")
		}

		// 列出 session 脚本
		sessionDir := filepath.Join(m.baseDir, "scripts", sessionID)
		files, err := os.ReadDir(sessionDir)
		if err != nil {
			if os.IsNotExist(err) {
				return scripts, nil
			}
			return nil, fmt.Errorf("read session dir: %w", err)
		}

		for _, f := range files {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".py") {
				info, err := f.Info()
				if err != nil {
					continue
				}
				scripts = append(scripts, ScriptInfo{
					Name:      strings.TrimSuffix(f.Name(), ".py"),
					Scope:     "session",
					SessionID: sessionID,
					Path:      filepath.Join(sessionDir, f.Name()),
					Size:      info.Size(),
					ModTime:   info.ModTime(),
				})
			}
		}
	} else {
		return nil, fmt.Errorf("invalid scope: %s", scope)
	}

	return scripts, nil
}

// GetScript 获取脚本内容
func (m *Manager) GetScript(name, scope, sessionID string) (string, error) {
	scriptPath, err := m.getScriptPath(name, scope, sessionID)
	if err != nil {
		return "", err
	}

	code, err := os.ReadFile(scriptPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("script not found: %s", name)
		}
		return "", fmt.Errorf("read script: %w", err)
	}

	return string(code), nil
}

// DeleteScript 删除脚本
func (m *Manager) DeleteScript(name, scope, sessionID string) error {
	scriptPath, err := m.getScriptPath(name, scope, sessionID)
	if err != nil {
		return err
	}

	if err := os.Remove(scriptPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("script not found: %s", name)
		}
		return fmt.Errorf("delete script: %w", err)
	}

	return nil
}

// GetScriptPath 获取脚本路径（供 Executor 使用）
func (m *Manager) GetScriptPath(name, scope, sessionID string) (string, error) {
	return m.getScriptPath(name, scope, sessionID)
}

// getScriptPath 内部方法：获取脚本路径
func (m *Manager) getScriptPath(name, scope, sessionID string) (string, error) {
	if err := validateScriptName(name); err != nil {
		return "", err
	}

	filename := name + ".py"

	switch scope {
	case "global":
		return filepath.Join(m.baseDir, "scripts", "global", filename), nil
	case "session":
		if sessionID == "" {
			return "", fmt.Errorf("session_id required for session scope")
		}
		return filepath.Join(m.baseDir, "scripts", sessionID, filename), nil
	default:
		return "", fmt.Errorf("invalid scope: %s", scope)
	}
}

// validateScriptName 验证脚本名
func validateScriptName(name string) error {
	if name == "" {
		return fmt.Errorf("script name cannot be empty")
	}

	// 禁止路径遍历
	if strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		return fmt.Errorf("invalid script name: %s", name)
	}

	// 只允许字母、数字、下划线、连字符
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return fmt.Errorf("invalid character in script name: %c", c)
		}
	}

	return nil
}
