package gemini

import "github.com/asaidimu/workspace/provider"

func catalogue() []provider.ModelInfo {
	return []provider.ModelInfo{
		// ── Flash‑Lite models – highest free‑tier RPM/RPD ────────────────────
		{
			ID: "gemini-3.5-flash-lite",
			Capabilities: provider.Capabilities{
				Streaming: true, NativeTools: true, StructuredOutput: true,
				Vision: true, PDF: true, Thinking: true, FileUpload: true,
				PromptCaching:   true,
				ContextWindow:   1_048_576,
				MaxOutputTokens: 65_536,
			},
			Pricing: provider.Pricing{Currency: "USD", InputMicrosPerMTok: 0, OutputMicrosPerMTok: 0, CachedMicrosPerMTok: 0},
		},
		{
			ID: "gemini-3.1-flash-lite",
			Capabilities: provider.Capabilities{
				Streaming: true, NativeTools: true, StructuredOutput: true,
				Vision: true, PDF: true, Thinking: true, FileUpload: true,
				PromptCaching:   true,
				ContextWindow:   1_048_576,
				MaxOutputTokens: 65_536,
			},
			Pricing: provider.Pricing{Currency: "USD", InputMicrosPerMTok: 0, OutputMicrosPerMTok: 0, CachedMicrosPerMTok: 0},
		},

		// ── Gemma 4 – open‑weight models, free tier ──────────────────────────
		{
			ID: "gemma-4-26b-a4b-it",
			Capabilities: provider.Capabilities{
				Streaming: true, NativeTools: true, StructuredOutput: true,
				Vision: true, PDF: true, Thinking: true, FileUpload: false,
				PromptCaching:   false,
				ContextWindow:   262_144,
				MaxOutputTokens: 32_768,
			},
			Pricing: provider.Pricing{Currency: "USD", InputMicrosPerMTok: 0, OutputMicrosPerMTok: 0, CachedMicrosPerMTok: 0},
		},
		{
			ID: "gemma-4-31b-it",
			Capabilities: provider.Capabilities{
				Streaming: true, NativeTools: true, StructuredOutput: true,
				Vision: true, PDF: true, Thinking: true, FileUpload: false,
				PromptCaching:   false,
				ContextWindow:   262_144,
				MaxOutputTokens: 32_768,
			},
			Pricing: provider.Pricing{Currency: "USD", InputMicrosPerMTok: 0, OutputMicrosPerMTok: 0, CachedMicrosPerMTok: 0},
		},

		// ── Standard Flash models – balanced free tier ────────────────────────
		{
			ID: "gemini-2.5-flash",
			Capabilities: provider.Capabilities{
				Streaming: true, NativeTools: true, StructuredOutput: true,
				Vision: true, PDF: true, Thinking: true, FileUpload: true,
				PromptCaching:   true,
				ContextWindow:   1_048_576,
				MaxOutputTokens: 65_536,
			},
			Pricing: provider.Pricing{Currency: "USD", InputMicrosPerMTok: 0, OutputMicrosPerMTok: 0, CachedMicrosPerMTok: 0},
		},
		{
			ID: "gemini-3.5-flash",
			Capabilities: provider.Capabilities{
				Streaming: true, NativeTools: true, StructuredOutput: true,
				Vision: true, PDF: true, Thinking: true, FileUpload: true,
				PromptCaching:   true,
				ContextWindow:   1_048_576,
				MaxOutputTokens: 65_536,
			},
			Pricing: provider.Pricing{Currency: "USD", InputMicrosPerMTok: 0, OutputMicrosPerMTok: 0, CachedMicrosPerMTok: 0},
		},
		{
			ID: "gemini-3.6-flash",
			Capabilities: provider.Capabilities{
				Streaming: true, NativeTools: true, StructuredOutput: true,
				Vision: true, PDF: true, Thinking: true, FileUpload: true,
				PromptCaching:   true,
				ContextWindow:   1_048_576,
				MaxOutputTokens: 65_536,
			},
			Pricing: provider.Pricing{Currency: "USD", InputMicrosPerMTok: 0, OutputMicrosPerMTok: 0, CachedMicrosPerMTok: 0},
		},
		{
			ID: "gemini-3.8-flash",
			Capabilities: provider.Capabilities{
				Streaming: true, NativeTools: true, StructuredOutput: true,
				Vision: true, PDF: true, Thinking: true, FileUpload: true,
				PromptCaching:   true,
				ContextWindow:   1_048_576,
				MaxOutputTokens: 65_536,
			},
			Pricing: provider.Pricing{Currency: "USD", InputMicrosPerMTok: 0, OutputMicrosPerMTok: 0, CachedMicrosPerMTok: 0},
		},

		// ── Pro model – very limited free quota ───────────────────────────────
		{
			ID: "gemini-2.5-pro",
			Capabilities: provider.Capabilities{
				Streaming: true, NativeTools: true, StructuredOutput: true,
				Vision: true, PDF: true, Thinking: true, FileUpload: true,
				PromptCaching:   true,
				ContextWindow:   1_048_576,
				MaxOutputTokens: 65_536,
			},
			Pricing: provider.Pricing{Currency: "USD", InputMicrosPerMTok: 0, OutputMicrosPerMTok: 0, CachedMicrosPerMTok: 0},
		},

	}
}
