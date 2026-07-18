// contract 包是 LOOM 内核与 LLM 世界之间唯一的纯接口桥：
// 内核五原语（State/Step/Router/Store/Graph）本身完全不知道 LLM 的存在，
// 所有与模型提供商相关的能力都被抽象成本包中提供商无关的接口与类型。
package contract

import "context" // 仅依赖标准库 context，保证 contract 包零第三方依赖

// Embedder generates vector embeddings from text.
// Embedder 是文本向量化的提供商无关抽象：任何嵌入模型（OpenAI、本地模型等）
// 只要实现这两个方法即可接入，供检索/记忆等上层组件使用。
type Embedder interface {
	// Embed converts one or more texts into embedding vectors.
	// Embed 批量把文本转成向量：入参与返回值按下标一一对应；
	// ctx 用于超时与取消控制，实现方必须尊重取消信号。
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Dimension returns the embedding vector dimension.
	// Dimension 返回向量维度（由模型决定的固定值），
	// 供向量存储在建索引前校验维度是否匹配，避免运行期才发现错配。
	Dimension() int
}
