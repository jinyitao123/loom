package stdlib

import (
	"context"       // Store 接口要求传入 context；本文件的读写为本地快速操作，使用 Background 即可
	"encoding/json" // 消息列表以 JSON 序列化后落入存储，保证跨进程/跨语言可读

	"github.com/jinyitao123/loom"          // 内核包：提供 Store 抽象
	"github.com/jinyitao123/loom/contract" // contract 包：提供提供商无关的 Message 类型
)

// SaveSession persists the conversation messages for a session.
// SaveSession 把一个会话的完整消息历史持久化到存储：
// 按 ("session", sessionID) 作为命名空间与键，整体覆盖写入（非增量追加）。
func SaveSession(store loom.Store, sessionID string, msgs []contract.Message) error {
	// 先把消息列表序列化为 JSON；失败（理论上仅在含不可序列化字段时发生）直接上抛。
	data, err := json.Marshal(msgs)
	if err != nil {
		return err
	}
	// 写入存储："session" 是固定命名空间，与检查点等其他数据隔离；键为会话 ID。
	return store.Put(context.Background(), "session", sessionID, data)
}

// LoadSession restores conversation messages for a session.
// Returns nil, nil if the session does not exist (not an error).
// LoadSession 恢复一个会话的消息历史。
// 约定：会话不存在返回 nil, nil 而不是错误——"没有历史"是新会话的正常起点，
// 调用方无需区分"首次对话"与"读取失败"两种分支。
func LoadSession(store loom.Store, sessionID string) ([]contract.Message, error) {
	// 从存储读取原始字节；任何读取错误都按"会话不存在"处理（见下）。
	data, err := store.Get(context.Background(), "session", sessionID)
	if err != nil {
		return nil, nil // no session = empty history
		// 有意吞掉错误：把"未找到"归一化为空历史，兑现上面的 nil, nil 约定。
	}
	// 反序列化回消息列表；数据存在但损坏（JSON 解析失败）才是真正的错误，必须上抛。
	var msgs []contract.Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, err
	}
	// 正常返回恢复出的历史消息。
	return msgs, nil
}
