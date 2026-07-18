package loom

import (
	"context" // 满足 Store 接口签名；内存实现本身不使用 ctx
	"fmt"     // 拼装“键不存在”的错误信息
	"sort"    // List 结果按字典序排序，符合 Store 协议约定
	"strings" // List 的键前缀匹配
	"sync"    // 读写锁，保证并发安全
)

// MemStore is an in-memory Store implementation for testing.
// MemStore 是内存版 Store 实现，仅供测试使用：不落盘、进程退出即失。
// 并发安全手段：一把 sync.RWMutex 保护两级 map——读操作持读锁可并行，写操作持写锁互斥。
type MemStore struct {
	mu   sync.RWMutex                 // 读写锁：所有导出方法都先取锁再进入 *Locked 无锁内核
	data map[string]map[string][]byte // 两级 map：命名空间 → (键 → 值字节)，实现 ns 隔离
}

// NewMemStore creates a new empty in-memory store.
// NewMemStore 创建一个空的内存存储。
func NewMemStore() *MemStore {
	return &MemStore{
		data: make(map[string]map[string][]byte), // 预建外层 map；内层桶在首次 Put 时懒创建
	}
}

// Get 并发安全读取：取读锁后委托给无锁内核。
func (s *MemStore) Get(_ context.Context, ns, key string) ([]byte, error) {
	s.mu.RLock()         // 读锁：允许多个读操作并行
	defer s.mu.RUnlock() // 返回时释放
	return s.getLocked(ns, key)
}

// getLocked 是 Get 的无锁内核（调用方必须已持锁）：查桶、查键、返回防御性拷贝。
func (s *MemStore) getLocked(ns, key string) ([]byte, error) {
	// 命名空间不存在与键不存在返回同样的“未找到”错误：调用方无需区分两种缺失。
	bucket, ok := s.data[ns]
	if !ok {
		return nil, fmt.Errorf("loom/memstore: key %q not found in namespace %q", key, ns)
	}
	val, ok := bucket[key] // 桶内查键
	if !ok {
		return nil, fmt.Errorf("loom/memstore: key %q not found in namespace %q", key, ns)
	}
	// 返回副本而非内部切片：防止调用方改写返回值污染存储内部数据。
	cp := make([]byte, len(val))
	copy(cp, val)
	return cp, nil
}

// Put 并发安全写入：取写锁后委托给无锁内核。
func (s *MemStore) Put(_ context.Context, ns, key string, value []byte) error {
	s.mu.Lock() // 写锁：与所有读写互斥
	defer s.mu.Unlock()
	return s.putLocked(ns, key, value)
}

// putLocked 是 Put 的无锁内核：懒建命名空间桶，存入调用方切片的副本。
func (s *MemStore) putLocked(ns, key string, value []byte) error {
	// 命名空间桶懒创建：首次向该 ns 写入时才分配。
	if s.data[ns] == nil {
		s.data[ns] = make(map[string][]byte)
	}
	// 存副本而非原切片：防止调用方事后修改切片影响已存数据（与 Get 的拷贝策略对称）。
	cp := make([]byte, len(value))
	copy(cp, value)
	s.data[ns][key] = cp // 覆盖写：同键新值替换旧值
	return nil
}

// Delete 并发安全删除：取写锁后委托给无锁内核。
func (s *MemStore) Delete(_ context.Context, ns, key string) error {
	s.mu.Lock() // 写锁互斥
	defer s.mu.Unlock()
	return s.deleteLocked(ns, key)
}

// deleteLocked 是 Delete 的无锁内核：删除不存在的键静默成功（幂等，符合 Store 协议）。
func (s *MemStore) deleteLocked(ns, key string) error {
	// 命名空间存在才需要删；内置 delete 对不存在的键本身就是空操作。
	if bucket, ok := s.data[ns]; ok {
		delete(bucket, key)
	}
	return nil
}

// List 并发安全列举：取读锁后委托给无锁内核。
func (s *MemStore) List(_ context.Context, ns, prefix string) ([]string, error) {
	s.mu.RLock() // 只读操作，用读锁即可
	defer s.mu.RUnlock()
	return s.listLocked(ns, prefix)
}

// listLocked 是 List 的无锁内核：收集 ns 下匹配前缀的键并按字典序返回。
func (s *MemStore) listLocked(ns, prefix string) ([]string, error) {
	// 命名空间不存在视为空结果而非错误（列举语义与 Get 的“必须存在”不同）。
	bucket, ok := s.data[ns]
	if !ok {
		return nil, nil
	}
	var keys []string
	// 遍历桶内全部键做前缀过滤；空前缀会匹配所有键。
	for k := range bucket {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys) // map 遍历无序，显式排序保证结果确定、可复现（测试友好）
	return keys, nil
}

// Tx runs a function inside a simulated transaction using copy-on-write.
// Tx 用“整库深拷贝 + 整体替换”模拟事务：持写锁期间在快照副本上执行 fn，
// fn 成功则把副本整体换入（提交），失败则丢弃副本（回滚），原数据全程不被触碰。
// 代价是 O(全库) 的拷贝且事务期间阻塞所有其他操作——仅测试场景可接受。
func (s *MemStore) Tx(_ context.Context, fn func(Store) error) error {
	s.mu.Lock() // 整个事务期间独占写锁，天然把所有事务序列化
	defer s.mu.Unlock()

	snapshot := s.cloneLocked() // 深拷贝出事务工作副本，fn 的一切读写都作用于副本
	// 执行事务体：报错即回滚——直接返回错误，副本被丢弃，原数据未变。
	if err := fn(snapshot); err != nil {
		return err
	}
	s.data = snapshot.data // 提交：持锁下整体替换底层数据指针，对外表现为原子生效
	return nil
}

// cloneLocked creates a deep copy of the store data. Must be called with mu held.
// cloneLocked 深拷贝全部数据（含每个值的字节副本）并包成事务视图；调用方必须已持有 mu。
func (s *MemStore) cloneLocked() *memTxStore {
	cloned := make(map[string]map[string][]byte, len(s.data)) // 新外层 map，容量与原库一致
	// 逐命名空间、逐键深拷贝：值字节也复制，确保事务内的修改不会透写回原库。
	for ns, bucket := range s.data {
		newBucket := make(map[string][]byte, len(bucket)) // 新桶
		for k, v := range bucket {
			cp := make([]byte, len(v)) // 值字节副本
			copy(cp, v)
			newBucket[k] = cp
		}
		cloned[ns] = newBucket
	}
	return &memTxStore{data: cloned} // 包装为事务视图返回
}

// memTxStore is the transactional view used inside MemStore.Tx.
// It does not need locking because it is only accessed by the Tx callback.
// memTxStore 是 MemStore.Tx 回调内部使用的事务视图：
// 外层 Tx 已持写锁、副本只被回调单线程访问，因此自身无需加锁；
// 各方法是 MemStore 对应无锁内核的简化版（省去锁与防御性拷贝）。
type memTxStore struct {
	data map[string]map[string][]byte // 事务工作副本；提交时被整体换入 MemStore
}

// Get 事务视图读取：逻辑同 getLocked，但直接返回内部切片（视图生命周期仅限事务，无需拷贝）。
func (s *memTxStore) Get(_ context.Context, ns, key string) ([]byte, error) {
	// 命名空间与键任一缺失都报“未找到”。
	bucket, ok := s.data[ns]
	if !ok {
		return nil, fmt.Errorf("loom/memstore: key %q not found in namespace %q", key, ns)
	}
	val, ok := bucket[key]
	if !ok {
		return nil, fmt.Errorf("loom/memstore: key %q not found in namespace %q", key, ns)
	}
	return val, nil // 直接返回内部切片：事务视图短命，简化实现
}

// Put 事务视图写入：懒建桶后直接存原切片（不拷贝，理由同上）。
func (s *memTxStore) Put(_ context.Context, ns, key string, value []byte) error {
	// 命名空间桶懒创建。
	if s.data[ns] == nil {
		s.data[ns] = make(map[string][]byte)
	}
	s.data[ns][key] = value // 直接引用调用方切片：事务提交后即成为正式数据
	return nil
}

// Delete 事务视图删除：幂等，键不存在静默成功。
func (s *memTxStore) Delete(_ context.Context, ns, key string) error {
	if bucket, ok := s.data[ns]; ok {
		delete(bucket, key) // delete 对不存在的键是空操作
	}
	return nil
}

// List 事务视图列举：逻辑同 listLocked，前缀过滤后按字典序返回。
func (s *memTxStore) List(_ context.Context, ns, prefix string) ([]string, error) {
	// 命名空间不存在视为空结果。
	bucket, ok := s.data[ns]
	if !ok {
		return nil, nil
	}
	var keys []string
	// 前缀过滤。
	for k := range bucket {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys) // 排序保证结果确定性
	return keys, nil
}

// Tx 在事务视图上再开事务即嵌套事务：按 Store 协议必须拒绝。
func (s *memTxStore) Tx(_ context.Context, _ func(Store) error) error {
	return ErrNestedTx // 返回哨兵错误，杜绝“看似成功实则无隔离”的伪嵌套事务
}
