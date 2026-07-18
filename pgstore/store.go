// Package pgstore implements loom.Store backed by PostgreSQL.
// 中文说明：本包提供 loom.Store（存储）契约的 PostgreSQL 实现——五个方法
// Get/Put/Delete/List/Tx 全部落在单表 loom_store 上。命名空间隔离靠复合
// 主键 (namespace, key) 实现：所有语句都以 namespace 作为第一过滤条件，
// 不同命名空间的同名 key 互不可见、互不冲突，无需分表或 schema 切换。
package pgstore

import (
	"context" // 所有数据库操作都透传调用方的上下文（超时/取消）
	"fmt"     // 错误包装，统一加 "pgstore:" 前缀便于定位

	"github.com/jackc/pgx/v5"         // pgx 核心类型：Tx、ErrNoRows
	"github.com/jackc/pgx/v5/pgxpool" // 连接池：并发安全，供长生命周期复用
	"github.com/jinyitao123/loom"     // Store 接口与 ErrNestedTx 哨兵错误
)

// PGStore implements loom.Store with PostgreSQL.
// 中文说明：池级实现——每次调用从连接池取连接执行，各方法相互独立、
// 自动提交；需要原子性时用 Tx 把多次操作绑进一个事务。
type PGStore struct {
	pool *pgxpool.Pool // 底层 pgx 连接池，Close 前始终有效
}

// New creates a new PGStore and verifies the connection.
// 中文说明：用连接串建池。注意 pgxpool.New 只校验配置、惰性建连，
// 真正的连通性要到首次查询（或调用方自行 Ping）才验证。
func New(connString string) (*PGStore, error) {
	// 建池用 Background 上下文：池的生命周期不该绑在某次请求的上下文上。
	pool, err := pgxpool.New(context.Background(), connString)
	if err != nil {
		return nil, fmt.Errorf("pgstore: connect: %w", err) // 配置/解析失败即报错
	}
	return &PGStore{pool: pool}, nil // 包装为 Store 实现返回
}

// Close releases the connection pool.
// 中文说明：关闭连接池、释放全部连接；关闭后任何方法调用都会失败。
func (s *PGStore) Close() {
	s.pool.Close()
}

// Migrate creates the loom_store table if it doesn't exist.
// 中文说明：幂等建表（IF NOT EXISTS），可在每次启动时安全重复调用。
// 表结构即命名空间隔离的载体：复合主键 (namespace, key) 保证同一命名
// 空间内 key 唯一；value 用 BYTEA 存任意字节（序列化格式由上层决定）；
// updated_at 仅作审计观测，读写路径不依赖它。
func (s *PGStore) Migrate(ctx context.Context) error {
	// 单条 DDL，无需事务包裹；Exec 忽略返回的命令标签。
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS loom_store (
			namespace TEXT NOT NULL,
			key       TEXT NOT NULL,
			value     BYTEA NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (namespace, key)
		)
	`)
	if err != nil {
		return fmt.Errorf("pgstore: migrate: %w", err) // 建表失败（权限/连接等）
	}
	return nil
}

// Get retrieves a value by namespace and key.
// 中文说明：按 (namespace, key) 精确取值；主键点查，最多一行。
// key 不存在返回带定位信息的错误（而非 nil 值），调用方以此区分
// "没有这个键"与"其他数据库故障"。
func (s *PGStore) Get(ctx context.Context, ns, key string) ([]byte, error) {
	var val []byte // 接收 BYTEA 列的目标缓冲
	// QueryRow + Scan：占位符参数化，杜绝 SQL 注入。
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM loom_store WHERE namespace=$1 AND key=$2`, ns, key,
	).Scan(&val)
	if err != nil {
		// 零行命中被 pgx 归一为 ErrNoRows，翻译成对调用方友好的 not found 错误。
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("pgstore: key %q not found in namespace %q", key, ns)
		}
		return nil, fmt.Errorf("pgstore: get: %w", err) // 其余为真正的查询失败
	}
	return val, nil // 命中：返回原始字节
}

// Put upserts a value.
// 中文说明：写入采用 upsert 语义——INSERT 撞上复合主键冲突时转为 UPDATE，
// 用 EXCLUDED.value 取"本次想插入的新值"覆盖旧值并刷新 updated_at。
// 一条语句原子完成"存在即改、不存在即建"，无需先查后写的竞态窗口。
func (s *PGStore) Put(ctx context.Context, ns, key string, val []byte) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO loom_store (namespace, key, value, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (namespace, key) DO UPDATE SET value=EXCLUDED.value, updated_at=NOW()`,
		ns, key, val)
	if err != nil {
		return fmt.Errorf("pgstore: put: %w", err) // 写入失败统一包装
	}
	return nil
}

// Delete removes a key.
// 中文说明：按 (namespace, key) 删除；键本不存在也视为成功（幂等删除），
// 不检查受影响行数。
func (s *PGStore) Delete(ctx context.Context, ns, key string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM loom_store WHERE namespace=$1 AND key=$2`, ns, key)
	if err != nil {
		return fmt.Errorf("pgstore: delete: %w", err) // 仅数据库层面失败才报错
	}
	return nil
}

// List returns keys matching a prefix within a namespace.
// 中文说明：列出命名空间内以 prefix 开头的全部 key，按字典序返回。
// 前缀匹配用 SQL 侧拼接 LIKE $2||'%' 实现，参数化传入 prefix；
// 边界：prefix 里的 LIKE 通配符（% 和 _）未做转义，含这两个字符的
// 前缀会被当作通配模式匹配。空 prefix 即列出该命名空间全部 key。
func (s *PGStore) List(ctx context.Context, ns, prefix string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT key FROM loom_store WHERE namespace=$1 AND key LIKE $2||'%'
		 ORDER BY key`, ns, prefix)
	if err != nil {
		return nil, fmt.Errorf("pgstore: list: %w", err) // 查询本身失败
	}
	defer rows.Close() // 确保游标释放，即使中途 Scan 出错

	var keys []string // 结果集；零命中时保持 nil
	// 逐行扫描 key 列。
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("pgstore: list scan: %w", err) // 单行解码失败即中止
		}
		keys = append(keys, k)
	}
	// rows.Err 捕获迭代过程中的延迟错误（如连接中断），必须在返回前检查。
	return keys, rows.Err()
}

// Tx executes fn within a PostgreSQL transaction.
// 中文说明：事务边界。语义协议——
//   - 传给回调 fn 的 Store 是绑定在本事务上的 pgTxStore，回调内的所有
//     读写都走同一个事务连接，具备原子性与事务隔离；
//   - 回调返回 nil → 提交（Commit）；返回错误 → 原样透传该错误并回滚；
//   - 不支持嵌套：在回调内再调 Tx 会得到 loom.ErrNestedTx。
func (s *PGStore) Tx(ctx context.Context, fn func(loom.Store) error) error {
	// 从池里开启事务；失败多为拿不到连接或服务端拒绝。
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore: begin tx: %w", err)
	}
	// 兜底回滚：回调报错或 panic 时保证事务不悬挂；
	// 若已成功 Commit，此处 Rollback 是无害的空操作（错误被有意忽略）。
	defer tx.Rollback(ctx) //nolint:errcheck

	// 把事务句柄包成 Store，注入回调——回调看到的是"同接口、事务版"的存储。
	txStore := &pgTxStore{tx: tx}
	// 回调返回错误：直接透传（不额外包装），由上面的 defer 完成回滚。
	if err := fn(txStore); err != nil {
		return err
	}
	// 回调成功：提交事务；提交本身的错误（如序列化冲突）返回给调用方。
	return tx.Commit(ctx)
}

// pgTxStore implements loom.Store within a single transaction.
// 中文说明：事务绑定版 Store——所有操作走 pgx.Tx 而非连接池，
// SQL 与错误语义同池级实现一一对应，仅错误前缀多标注 "tx"。
// 生命周期仅限 Tx 回调期间，不得逃逸保存。
type pgTxStore struct {
	tx pgx.Tx // 由外层 Tx 开启并负责提交/回滚的事务句柄
}

// Get：事务内点查，语义同 PGStore.Get（含 not found 翻译），读到的是事务视图。
func (s *pgTxStore) Get(ctx context.Context, ns, key string) ([]byte, error) {
	var val []byte // 接收 BYTEA 列
	// 同一条主键点查 SQL，改由事务句柄执行。
	err := s.tx.QueryRow(ctx,
		`SELECT value FROM loom_store WHERE namespace=$1 AND key=$2`, ns, key,
	).Scan(&val)
	if err != nil {
		// 零行命中 → not found；错误文案与池级实现保持一致。
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("pgstore: key %q not found in namespace %q", key, ns)
		}
		return nil, fmt.Errorf("pgstore: tx get: %w", err) // 其余查询失败
	}
	return val, nil
}

// Put：事务内 upsert，语义同 PGStore.Put；写入仅在事务提交后对外可见。
func (s *pgTxStore) Put(ctx context.Context, ns, key string, val []byte) error {
	// 同一条 ON CONFLICT upsert SQL，走事务句柄。
	_, err := s.tx.Exec(ctx,
		`INSERT INTO loom_store (namespace, key, value, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (namespace, key) DO UPDATE SET value=EXCLUDED.value, updated_at=NOW()`,
		ns, key, val)
	if err != nil {
		return fmt.Errorf("pgstore: tx put: %w", err)
	}
	return nil
}

// Delete：事务内幂等删除，语义同 PGStore.Delete。
func (s *pgTxStore) Delete(ctx context.Context, ns, key string) error {
	_, err := s.tx.Exec(ctx,
		`DELETE FROM loom_store WHERE namespace=$1 AND key=$2`, ns, key)
	if err != nil {
		return fmt.Errorf("pgstore: tx delete: %w", err)
	}
	return nil
}

// List：事务内前缀列举，语义同 PGStore.List（含 LIKE 通配符未转义的边界）。
func (s *pgTxStore) List(ctx context.Context, ns, prefix string) ([]string, error) {
	rows, err := s.tx.Query(ctx,
		`SELECT key FROM loom_store WHERE namespace=$1 AND key LIKE $2||'%'
		 ORDER BY key`, ns, prefix)
	if err != nil {
		return nil, fmt.Errorf("pgstore: tx list: %w", err)
	}
	defer rows.Close() // 事务内同样必须释放游标

	var keys []string
	// 逐行扫描，流程与池级 List 完全一致。
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("pgstore: tx list scan: %w", err)
		}
		keys = append(keys, k)
	}
	// 返回前检查迭代期延迟错误。
	return keys, rows.Err()
}

// Tx returns ErrNestedTx — PostgreSQL nested transactions require SAVEPOINTs
// which are not supported in this simple implementation.
// 中文说明：显式拒绝嵌套事务。PostgreSQL 的"事务中开事务"需借助 SAVEPOINT
// 模拟，本实现刻意不做，直接返回 loom.ErrNestedTx 哨兵错误让调用方感知，
// 而不是静默复用外层事务造成语义混淆。
func (s *pgTxStore) Tx(_ context.Context, _ func(loom.Store) error) error {
	return loom.ErrNestedTx
}

// Pool returns the underlying connection pool (for platform extensions).
// 中文说明：暴露底层连接池的逃生口，供平台扩展在同一池上跑自定义 SQL；
// 绕过 Store 契约使用时，命名空间隔离等约定需扩展方自行遵守。
func (s *PGStore) Pool() *pgxpool.Pool {
	return s.pool
}

// Compile-time interface check.
// 编译期断言：两种实现都必须完整满足 loom.Store 接口，接口变更时在编译期即暴露。
var (
	_ loom.Store = (*PGStore)(nil)
	_ loom.Store = (*pgTxStore)(nil)
)
