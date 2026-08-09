package capture

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"gta/pkg/event"
)

// State 表示 Source 的生命周期状态。
type State int

const (
	StateCreated State = iota
	StateRunning
	StateClosed
)

func (s State) String() string {
	switch s {
	case StateCreated:
		return "created"
	case StateRunning:
		return "running"
	case StateClosed:
		return "closed"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// ErrAlreadyStarted 表示 Source 已经启动。
var ErrAlreadyStarted = errors.New("source already started")

// Source 是流量来源抽象。每个 Source 只输出 event.Packet，不暴露 Connection/Session。
type Source interface {
	// Start 启动 Source，开始往 Packets() channel 中发送数据。
	// 非幂等：重复调用返回 ErrAlreadyStarted。
	Start(ctx context.Context) error

	// Packets 返回只读 Packet 通道。通道在 Source 关闭后关闭。
	Packets() <-chan event.Packet

	// Err 在 Packets() 关闭后返回运行期错误。EOF 返回 nil。
	Err() error

	// Close 主动关闭 Source，释放资源。幂等。
	Close() error

	// Stats 返回累计统计值，生命周期结束后仍可读取最终值。
	Stats() Stats

	// State 返回当前生命周期状态。
	State() State
}

// Stats 是 Source 的累计统计。
type Stats struct {
	PacketsIn  uint64
	PacketsOut uint64
	BytesIn    uint64
	BytesOut   uint64
	Drops      uint64
	Blocked    uint64
	Errors     uint64
	Extra      map[string]any
}

// Factory 用于根据配置构造 Source。
type Factory interface {
	// Validate 校验配置是否合法。
	Validate(cfg any) error

	// New 根据配置构造 Source，但不启动。
	New(cfg any) (Source, error)
}

// FactoryFunc 允许用普通函数实现 Factory。
type FactoryFunc struct {
	ValidateFunc func(cfg any) error
	NewFunc      func(cfg any) (Source, error)
}

func (f FactoryFunc) Validate(cfg any) error {
	if f.ValidateFunc == nil {
		return nil
	}
	return f.ValidateFunc(cfg)
}

func (f FactoryFunc) New(cfg any) (Source, error) {
	return f.NewFunc(cfg)
}

var (
	factories = make(map[string]Factory)
	mu        sync.RWMutex
)

// Register 注册一个 Source Factory。重复注册会 panic。
func Register(name string, factory Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := factories[name]; ok {
		panic(fmt.Sprintf("source factory %q already registered", name))
	}
	factories[name] = factory
}

// New 根据名称和配置构造 Source，但不启动。内部会先调用 Validate。
func New(name string, cfg any) (Source, error) {
	mu.RLock()
	factory, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown source %q", name)
	}
	if err := factory.Validate(cfg); err != nil {
		return nil, fmt.Errorf("validate source %q config: %w", name, err)
	}
	return factory.New(cfg)
}

// Open 构造并启动 Source。等价于 New + Start。
func Open(ctx context.Context, name string, cfg any) (Source, error) {
	s, err := New(name, cfg)
	if err != nil {
		return nil, err
	}
	if err := s.Start(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// RegisteredNames 返回已注册的 Source 名称列表（无序）。
func RegisteredNames() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(factories))
	for name := range factories {
		names = append(names, name)
	}
	return names
}

// 标准 Metadata key，由各 Source 实现选择性填充。
const (
	MetaSource      = "source"       // Source 名称，如 "pcap-live"
	MetaInterface   = "interface"    // 网络接口名
	MetaDevice      = "device"       // 设备标识（与 interface 区分，可用于移动设备）
	MetaCaptureName = "capture_name" // 本次抓取的命名标识
	MetaClientAddr  = "client_addr"  // 代理/移动场景下的客户端地址
	MetaServerAddr  = "server_addr"  // 代理/移动场景下的服务端地址
	MetaAppPackage  = "app_package"  // Android 等场景下的应用包名
	MetaProcessName = "process_name" // 进程名
	MetaTruncated   = "truncated"    // 包被 SnapLen 截断（CaptureLength < OriginalLength）
)
