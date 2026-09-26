package stdlib

import (
	"encoding/json"
	"fmt" // 类型不匹配时构造带实际类型的错误信息

	"github.com/jinyitao123/loom"          // 内核包：State 就是 map[string]any
	"github.com/jinyitao123/loom/contract" // contract 包：Message/Usage 类型
)

// GetMessages retrieves conversation history with type checking.
// GetMessages 从状态中取出对话历史并做类型归一化。
// 难点在于：状态可能刚在内存中构造（值是强类型 []contract.Message），
// 也可能刚从检查点经 JSON 反序列化恢复（值退化成 []any 套 map[string]any），
// 本函数把两种形态统一还原为 []contract.Message。
func GetMessages(s loom.State) ([]contract.Message, error) {
	// 键不存在按"无历史"处理，返回 nil, nil 而非错误（与会话不存在的约定一致）。
	raw, ok := s["messages"]
	if !ok {
		return nil, nil
	}
	// Handle both typed and untyped (post-JSON roundtrip) messages.
	// 按实际动态类型分派处理两种形态。
	switch msgs := raw.(type) {
	case []contract.Message:
		// 内存直连路径：本就是强类型，直接返回，零开销。
		return msgs, nil
	case []any:
		// After JSON roundtrip, messages are []any of map[string]any.
		// The assistant's tool_calls must survive beside their tool results.
		result := make([]contract.Message, 0, len(msgs))
		for index, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				encoded, err := json.Marshal(mm)
				if err != nil {
					return nil, fmt.Errorf("loom: encode message %d: %w", index, err)
				}
				var msg contract.Message
				if err := json.Unmarshal(encoded, &msg); err != nil {
					return nil, fmt.Errorf("loom: decode message %d: %w", index, err)
				}
				result = append(result, msg)
			} else if typed, ok := m.(contract.Message); ok {
				// 兼容混合形态：[]any 里也可能直接装着强类型 Message（部分手工构造场景）。
				result = append(result, typed)
			} else {
				return nil, fmt.Errorf("loom: message %d is %T, want Message", index, m)
			}
		}
		return result, nil
	default:
		// 键存在但类型完全不对：这是调用方的编程错误，必须显式报错并带上实际类型。
		return nil, fmt.Errorf("loom: messages is %T, want []Message", raw)
	}
}

// MustGetMessages panics on type mismatch.
// MustGetMessages 是 GetMessages 的 panic 版：类型不匹配直接崩溃。
// 适用于"类型错误即编程错误"的场景（如初始化期），省去调用方的错误分支。
func MustGetMessages(s loom.State) []contract.Message {
	msgs, err := GetMessages(s)
	if err != nil {
		panic(err) // 类型错误属于程序缺陷，快速失败暴露问题
	}
	return msgs
}

// GetString retrieves a string value with a default fallback.
// GetString 从状态取字符串：键缺失或类型不符时返回调用方给定的兜底值，
// 让调用侧一行代码完成"取值+校验+默认"三件事。
func GetString(s loom.State, key, fallback string) string {
	if v, ok := s[key].(string); ok {
		return v
	}
	return fallback
}

// GetFloat retrieves a float64 value with a default fallback.
// GetFloat 从状态取 float64，缺失/类型不符返回兜底值。
// 注意：JSON 反序列化后所有数字都是 float64，所以数值统一走这个访问器。
func GetFloat(s loom.State, key string, fallback float64) float64 {
	if v, ok := s[key].(float64); ok {
		return v
	}
	return fallback
}

// GetBool retrieves a bool value with a default fallback.
// GetBool 从状态取布尔值，缺失/类型不符返回兜底值。
func GetBool(s loom.State, key string, fallback bool) bool {
	if v, ok := s[key].(bool); ok {
		return v
	}
	return fallback
}

// SetOutput is the type-safe setter for agent output.
// SetOutput 是 agent 输出的类型安全写入器：统一以 "output"/"usage" 两个约定键
// 构造状态增量，让下游步骤与预算钩子按固定键读取，避免各处手写魔法字符串。
func SetOutput(content string, usage contract.Usage) loom.State {
	return loom.State{"output": content, "usage": usage}
}
