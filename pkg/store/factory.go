package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"gta/pkg/schema"

	_ "github.com/jackc/pgx/v5/stdlib" // 注册 "pgx" driver
)

// Driver 标识事件/控制元数据的存储后端。
type Driver string

const (
	// DriverSQLite 是默认后端：每会话一个 capture.sqlite 文件 + 全局 control.sqlite。
	DriverSQLite Driver = "sqlite"
	// DriverPostgres 事件与控制元数据共用一个 PG 库，按 session_id 隔离会话行。
	DriverPostgres Driver = "postgres"
)

// IsPostgres 判断 driver 字符串是否表示 PostgreSQL 后端。
func IsPostgres(driver string) bool { return Driver(driver) == DriverPostgres }

// pgPoolCache 按 DSN 缓存共享的 *sql.DB，避免每个会话都新建连接池。
// PG 模式下一个库承载全部会话，连接池应在进程内共享。
var pgPoolCache sync.Map // map[string]*sql.DB

// openPG 打开（或复用）一个 PG 连接池。同一 DSN 首次创建时幂等建表。
func openPG(dsn string) (*sql.DB, error) {
	if v, ok := pgPoolCache.Load(dsn); ok {
		return v.(*sql.DB), nil
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	// LoadOrStore：并发首次打开同一 DSN 时，落败方关闭自己的池、复用胜方。
	if actual, loaded := pgPoolCache.LoadOrStore(dsn, db); loaded {
		_ = db.Close()
		return actual.(*sql.DB), nil
	}
	// 首次创建该 DSN 的连接池：幂等建表（CREATE TABLE IF NOT EXISTS）。
	if err := ensurePGSchema(context.Background(), db); err != nil {
		_ = db.Close()
		pgPoolCache.Delete(dsn)
		return nil, fmt.Errorf("init postgres schema: %w", err)
	}
	return db, nil
}

// OpenCaptureStore 打开（或创建）指定会话的事件存储。
//
// driver=="sqlite"：dsnOrPath 为该会话的 capture.sqlite 文件路径（每会话独立文件），
// sessionID 参数被忽略。
//
// driver=="postgres"：dsnOrPath 为共享 PG 连接串；sessionID 隔离该会话的全部行
// （raw_packets / events / state_changes / event_index 均带 session_id）。
func OpenCaptureStore(driver, dsnOrPath string, schemaReg *schema.Registry, sessionID string) (Store, error) {
	if schemaReg == nil {
		schemaReg = schema.NewRegistry()
	}
	if IsPostgres(driver) {
		db, err := openPG(dsnOrPath)
		if err != nil {
			return nil, err
		}
		return &PGStore{db: db, schemaReg: schemaReg, sessionID: sessionID}, nil
	}
	return NewSQLiteStore(dsnOrPath, schemaReg)
}

// OpenCaptureStoreReadOnly 以只读方式打开指定会话的事件存储（供 test_plugin 等采样场景）。
func OpenCaptureStoreReadOnly(driver, dsnOrPath string, schemaReg *schema.Registry, sessionID string) (Store, error) {
	if schemaReg == nil {
		schemaReg = schema.NewRegistry()
	}
	if IsPostgres(driver) {
		db, err := openPG(dsnOrPath)
		if err != nil {
			return nil, err
		}
		return &PGStore{db: db, schemaReg: schemaReg, sessionID: sessionID, readOnly: true}, nil
	}
	return NewSQLiteStoreReadOnly(dsnOrPath, schemaReg)
}

// ControlStoreBackend 是控制元数据存储的统一接口（SQLite / PG 双实现）。
// 调用方（gta-pipeline / gta-mcp）统一持有该接口，不感知具体后端。
type ControlStoreBackend interface {
	SessionStore
	// ListSessionsForProject 列出某项目下的全部会话（无 owner 过滤；
	// 项目是协作边界，调用方必须先完成 ActionProjectRead 鉴权）。
	ListSessionsForProject(ctx context.Context, projectID string, f SessionOwnerFilter) ([]SessionMeta, error)
	// ReconcileRunningSessions 把上一进程残留的 running 会话标记为 stopped。
	ReconcileRunningSessions(ctx context.Context, stoppedAt time.Time) (int64, error)
	// SetSessionProject 把会话绑定到某项目（Deprecated：无鉴权裸更新，仅限内部）。
	SetSessionProject(ctx context.Context, sessionID, projectID string) error
	// MoveSessionToProject 是 move_session_to_project 的原子落点（带租户 CAS）。
	MoveSessionToProject(ctx context.Context, sessionID, projectID, expectTenant string) error
	// RecordDebugAccess 追加一条 plugin_debug_access 审计行。
	RecordDebugAccess(ctx context.Context, d DebugAccess) (int64, error)
	// DebugAccesses 返回某会话的审计行（最新在前）。
	DebugAccesses(ctx context.Context, sessionID string) ([]DebugAccess, error)
	// 探针注册表（probes / probe_archive_segments，见 probe_store.go）。
	UpsertProbe(ctx context.Context, m ProbeMeta) error
	GetProbe(ctx context.Context, probeID string) (*ProbeMeta, error)
	GetProbeByTokenHash(ctx context.Context, tokenHash string) (*ProbeMeta, error)
	ListProbes(ctx context.Context) ([]ProbeMeta, error)
	UpdateProbeStatus(ctx context.Context, probeID string, st ProbeRuntimeStatus) error
	SetProbeConnection(ctx context.Context, probeID, state string, seen time.Time) error
	RenameProbe(ctx context.Context, probeID, name string) error
	RevokeProbe(ctx context.Context, probeID string) error
	DeleteProbe(ctx context.Context, probeID string) error
	ReplaceProbeSegments(ctx context.Context, probeID string, segs []ArchiveSegmentMeta) error
	ListProbeSegments(ctx context.Context, probeID string, fromMs, toMs int64) ([]ArchiveSegmentMeta, error)
	Close() error
	DB() *sql.DB
}

var (
	_ ControlStoreBackend = (*ControlStore)(nil)
	_ ControlStoreBackend = (*PGControlStore)(nil)
)

// OpenControlStore 打开控制元数据存储（sessions + plugin_debug_access）。
//
// driver=="sqlite"：dsnOrPath 为 control.sqlite 文件路径（projects / access_codes
// 等同文件共置）。
//
// driver=="postgres"：dsnOrPath 为共享 PG 连接串；sessions 与 plugin_debug_access
// 落到 PG。注意：projects / access_codes 等组织-访问子系统仍使用本地 sqlite 文件
// （见 gta-mcp/main.go 的装配逻辑），不在本次 PG 化范围内。
func OpenControlStore(driver, dsnOrPath string) (ControlStoreBackend, error) {
	if IsPostgres(driver) {
		db, err := openPG(dsnOrPath)
		if err != nil {
			return nil, err
		}
		return &PGControlStore{db: db}, nil
	}
	return NewControlStore(dsnOrPath)
}
