package main

import (
	"context"       // 满足 Store 接口签名；文件实现实际不消费取消语义
	"errors"        // errors.Is 判定文件不存在；构造嵌套 Tx 的哨兵错误
	"fmt"           // 构造语义化的"key 未找到"错误
	"io/fs"         // fs.ErrNotExist：跨平台的"文件不存在"哨兵
	"net/url"       // PathEscape/PathUnescape：任意 ns/key 与文件名之间的安全双向映射
	"os"            // 文件读写、目录创建与枚举
	"path/filepath" // 拼接目录/文件路径
	"strings"       // 前缀匹配与 .json 后缀裁剪
	"sync"          // 互斥锁：串行化进程内并发访问

	"github.com/jinyitao123/loom" // loom.Store 接口定义
)

// fileStore is a minimal persistent loom.Store backed by JSON files under a
// directory (one file per ns/key). It gives `loom run` cross-invocation session
// continuity (--resume) without a database — the daemon reuses the agent workdir
// across turns, so .loom-sessions/ persists there. For multi-node / heavy use,
// loom/pgstore is the production backend.
// 中文补充：fileStore 是单机文件版的 loom.Store（存储）：命名空间映射为
// 一个子目录、key 映射为其中一个 .json 文件。它让 `loom run` 不依赖数据库
// 即可跨进程续接会话（--resume）——守护进程跨轮次复用 agent 工作目录，
// .loom-sessions/ 因此得以留存。多节点/高负载场景应使用生产后端 loom/pgstore。
type fileStore struct {
	dir  string     // 存储根目录（每个命名空间对应一个子目录）
	mu   sync.Mutex // 串行化进程内并发读写（工具可能并行触发存储访问）
	inTx bool       // 标记"事务视图"：用于按契约拒绝嵌套 Tx
}

// newFileStore 构造以 dir 为根的文件存储；目录按需惰性创建（见 Put）。
func newFileStore(dir string) *fileStore { return &fileStore{dir: dir} }

// nsDir 把命名空间映射为子目录；PathEscape 保证任意字符的 ns 都是合法且无歧义的目录名。
func (f *fileStore) nsDir(ns string) string { return filepath.Join(f.dir, url.PathEscape(ns)) }

// path 把 (ns, key) 映射为具体文件：<dir>/<escape(ns)>/<escape(key)>.json。
func (f *fileStore) path(ns, key string) string {
	return filepath.Join(f.nsDir(ns), url.PathEscape(key)+".json")
}

// Get 读取一个 key 的原始字节；键不存在时返回带 ns/key 的语义化错误。
func (f *fileStore) Get(_ context.Context, ns, key string) ([]byte, error) {
	f.mu.Lock() // 与写操作互斥，避免读到写了一半的文件
	defer f.mu.Unlock()
	b, err := os.ReadFile(f.path(ns, key))
	// 文件不存在 → 转换为语义化的"key 未找到"错误（不向调用方泄漏文件系统细节）。
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("loom filestore: key %q/%q not found", ns, key)
	}
	return b, err // 其余情况：内容与错误原样返回
}

// Put 写入（或整体覆盖）一个 key；命名空间目录按需创建。
func (f *fileStore) Put(_ context.Context, ns, key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.path(ns, key) // 目标文件路径
	// 惰性创建命名空间目录；0700：会话数据仅允许属主访问。
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	// 0600 同样收紧文件权限；整文件覆盖写，value 即该 key 的完整新值。
	return os.WriteFile(p, value, 0o600)
}

// Delete 删除一个 key；删除不存在的 key 视为成功（幂等语义）。
func (f *fileStore) Delete(_ context.Context, ns, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := os.Remove(f.path(ns, key))
	// 目标本就不存在：幂等处理，不算错误。
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// List 枚举命名空间下匹配指定前缀的所有 key（对文件名反转义后再比较）。
func (f *fileStore) List(_ context.Context, ns, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := os.ReadDir(f.nsDir(ns))
	// 命名空间目录尚未创建 = 空命名空间：返回空列表而非错误。
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var keys []string // 命中的 key 集合
	for _, e := range entries {
		// 反向还原 key：先去掉 .json 后缀，再做 URL 反转义。
		name := strings.TrimSuffix(e.Name(), ".json")
		key, derr := url.PathUnescape(name)
		// 无法反转义的文件并非本存储写入，直接跳过（容忍目录中的杂质文件）。
		if derr != nil {
			continue
		}
		// 前缀过滤作用在还原后的 key 上（而非转义后的文件名）。
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// Tx 的实现取舍：文件存储面向单进程场景，不提供真正的原子性与回滚——
// 只用一个打了 inTx 标记的视图执行回调，以便按 Store 契约拒绝嵌套事务。
func (f *fileStore) Tx(ctx context.Context, fn func(loom.Store) error) error {
	// 已处于事务视图中再开 Tx：按契约显式拒绝，避免调用方误以为有嵌套事务语义。
	if f.inTx {
		return errors.New("loom filestore: nested Tx not supported")
	}
	// No real transaction semantics — the file store is single-process. Run the
	// function against a tx-marked view so nested Tx is rejected per the contract.
	// 中文补充：没有真正的事务语义——回调内的写入立即落盘、不可回滚；
	// 这里仅构造一个 inTx=true 的同目录视图，让嵌套 Tx 能被上面的检查拦住。
	tx := &fileStore{dir: f.dir, inTx: true}
	return fn(tx) // 直接在"事务"视图上执行回调，其错误原样上抛
}

// 编译期断言：fileStore 必须完整实现 loom.Store 接口，缺方法即编译失败。
var _ loom.Store = (*fileStore)(nil)
