package contract

import "context"

// Embedder generates vector embeddings from text.
type Embedder interface {
	// Embed converts one or more texts into embedding vectors.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Dimension returns the embedding vector dimension.
	Dimension() int
}
