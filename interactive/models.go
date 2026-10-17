package interactive

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// ModelCost is the price per million tokens.
type ModelCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// Model describes one selectable model.
type Model struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	API           string    `json:"api"`
	Provider      string    `json:"provider"`
	BaseURL       string    `json:"baseUrl"`
	Reasoning     bool      `json:"reasoning"`
	Input         []string  `json:"input"`
	ContextWindow int       `json:"contextWindow"`
	MaxTokens     int       `json:"maxTokens"`
	Cost          ModelCost `json:"cost"`
}

// Catalog is the set of configured models and the API key of each provider.
type Catalog struct {
	Models []Model
	keys   map[string]string
}

// Key returns the API key configured for a provider, or "".
func (c *Catalog) Key(provider string) string { return c.keys[provider] }

// Find returns the model with the given provider and id. An empty provider
// matches the first model with that id.
func (c *Catalog) Find(provider, id string) (Model, bool) {
	for _, m := range c.Models {
		if m.ID == id && (provider == "" || m.Provider == provider) {
			return m, true
		}
	}
	return Model{}, false
}

type modelsFile struct {
	Providers map[string]struct {
		BaseURL string  `json:"baseUrl"`
		API     string  `json:"api"`
		APIKey  string  `json:"apiKey"`
		Models  []Model `json:"models"`
	} `json:"providers"`
}

type authFile map[string]struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

var envName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// LoadCatalog reads models.json and auth.json from an agent configuration
// directory. Keys in auth.json take precedence over apiKey in models.json; a
// key that looks like an environment variable name and is set in the
// environment resolves to that variable's value. Missing files yield an empty
// catalog.
// LoadCatalog 读取配置目录下的 models.json 与 auth.json；auth.json 的密钥优先，
// 形如环境变量名且已设置时取环境变量的值；文件缺失时返回空目录。
func LoadCatalog(dir string, getenv func(string) string) (*Catalog, error) {
	catalog := &Catalog{keys: map[string]string{}}
	var models modelsFile
	if err := readJSON(filepath.Join(dir, "models.json"), &models); err != nil {
		return nil, err
	}
	var auth authFile
	if err := readJSON(filepath.Join(dir, "auth.json"), &auth); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(models.Providers))
	for name := range models.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		provider := models.Providers[name]
		api := provider.API
		if api == "" {
			api = "openai-completions"
		}
		for _, m := range provider.Models {
			m.Provider = name
			if m.API == "" {
				m.API = api
			}
			if m.BaseURL == "" {
				m.BaseURL = provider.BaseURL
			}
			if m.Name == "" {
				m.Name = m.ID
			}
			if len(m.Input) == 0 {
				m.Input = []string{"text"}
			}
			if m.ContextWindow == 0 {
				m.ContextWindow = 128000
			}
			if m.MaxTokens == 0 {
				m.MaxTokens = 16384
			}
			catalog.Models = append(catalog.Models, m)
		}
		if provider.APIKey != "" {
			catalog.keys[name] = resolveKey(provider.APIKey, getenv)
		}
	}
	for name, credential := range auth {
		if credential.Type == "api_key" && credential.Key != "" {
			catalog.keys[name] = resolveKey(credential.Key, getenv)
		}
	}
	return catalog, nil
}

func resolveKey(value string, getenv func(string) string) string {
	if envName.MatchString(value) {
		if v := getenv(value); v != "" {
			return v
		}
	}
	return value
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("interactive: parse %s: %w", path, err)
	}
	return nil
}
