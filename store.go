package loom

import "context" // 所有存储操作接受 context，取消/超时可透传到底层数据库驱动

// Store provides durable key-value storage with namespace isolation.
// Store（存储）是引擎的持久化抽象：带命名空间（ns）隔离的 KV 接口。
// 引擎用它落检查点（命名空间固定为 "checkpoint:<图名>"），stdlib 用它存记忆等长期数据；
// 命名空间隔离保证不同用途的数据互不污染。实现方（SQLite/Postgres/内存）只需满足此接口。
type Store interface {
	Get(ctx context.Context, ns string, key string) ([]byte, error)       // 读取 ns 下 key 的值；协议约定键不存在必须返回错误（而非 nil, nil）
	Put(ctx context.Context, ns string, key string, value []byte) error   // 写入或覆盖 ns 下 key 的值；实现方应保证写入的持久性
	Delete(ctx context.Context, ns string, key string) error              // 删除 ns 下的 key；删除不存在的键视为成功（幂等语义）
	List(ctx context.Context, ns string, prefix string) ([]string, error) // 列出 ns 下以 prefix 开头的全部键，约定按字典序返回（结果确定可复现）

	// Tx runs a function inside a database transaction.
	// The Store passed to fn is bound to the transaction.
	// If fn returns nil, the transaction commits. If fn returns an error,
	// the transaction rolls back.
	// Nested Tx calls are NOT supported and must return an error.
	// Tx 在数据库事务内执行 fn：传给 fn 的 Store 已绑定到该事务，
	// fn 返回 nil 则整体提交、返回 error 则整体回滚——调用方以此获得多次读写的原子性。
	// 协议约束：不支持嵌套事务，在事务内再调 Tx 必须返回 ErrNestedTx，实现方不得静默忽略。
	Tx(ctx context.Context, fn func(Store) error) error
}
