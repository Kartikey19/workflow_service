package providers

// Kind is a provider a connection can use. Anthropic, OpenAI and Gemini have
// their own API and client; every other kind speaks the OpenAI chat API at a
// known endpoint. The list follows the providers omp supports with an API key
// (subscription logins such as Copilot or Codex, and cloud accounts such as
// Bedrock, Vertex and Azure, are left out); Custom covers any other server.
type Kind struct {
	ID      string
	Title   string
	BaseURL string // default endpoint of an OpenAI-compatible kind
	// KeyOptional is set for servers that may run without a key, such as a
	// local Ollama.
	KeyOptional bool
}

// Native is true for kinds with their own API rather than the OpenAI one.
func (k Kind) Native() bool { return k.ID == Anthropic || k.ID == OpenAI || k.ID == Gemini }

// Catalog lists every kind, in the order the editor shows them.
var Catalog = []Kind{
	{ID: Anthropic, Title: "Anthropic"},
	{ID: OpenAI, Title: "OpenAI"},
	{ID: Gemini, Title: "Google Gemini"},
	{ID: Ollama, Title: "Ollama", BaseURL: "http://127.0.0.1:11434/v1", KeyOptional: true},
	{ID: "ollama_cloud", Title: "Ollama Cloud", BaseURL: "https://ollama.com/v1"},
	{ID: "openrouter", Title: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1"},
	{ID: "groq", Title: "Groq", BaseURL: "https://api.groq.com/openai/v1"},
	{ID: "deepseek", Title: "DeepSeek", BaseURL: "https://api.deepseek.com"},
	{ID: "mistral", Title: "Mistral", BaseURL: "https://api.mistral.ai/v1"},
	{ID: "xai", Title: "xAI", BaseURL: "https://api.x.ai/v1"},
	{ID: "together", Title: "Together AI", BaseURL: "https://api.together.xyz/v1"},
	{ID: "cerebras", Title: "Cerebras", BaseURL: "https://api.cerebras.ai/v1"},
	{ID: "moonshot", Title: "Moonshot (Kimi)", BaseURL: "https://api.moonshot.ai/v1"},
	{ID: "minimax", Title: "MiniMax", BaseURL: "https://api.minimax.io/v1"},
	{ID: "zai", Title: "Z.ai (GLM)", BaseURL: "https://api.z.ai/api/paas/v4"},
	{ID: "qwen", Title: "Qwen (Alibaba Cloud)", BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"},
	{ID: "huggingface", Title: "Hugging Face", BaseURL: "https://router.huggingface.co/v1"},
	{ID: "nvidia", Title: "NVIDIA", BaseURL: "https://integrate.api.nvidia.com/v1"},
	{ID: "venice", Title: "Venice", BaseURL: "https://api.venice.ai/api/v1"},
	{ID: "nanogpt", Title: "NanoGPT", BaseURL: "https://nano-gpt.com/api/v1"},
	{ID: "zenmux", Title: "ZenMux", BaseURL: "https://zenmux.ai/api/v1"},
	{ID: OpenAICompatible, Title: "Custom (OpenAI-compatible)", KeyOptional: true},
}

func lookup(id string) (Kind, bool) {
	for _, k := range Catalog {
		if k.ID == id {
			return k, true
		}
	}
	return Kind{}, false
}
