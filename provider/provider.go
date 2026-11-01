// Package provider defines the one interface every model sits behind. It
// streams typed events, calls tools natively, reports usage and never leaks
// vendor types into the core.
//
// The prompt pipeline produces a Request, the adapter talks to the vendor,
// and the agent loop consumes the Stream. Adapters use plain net/http and
// one shared SSE decoder; vendor SDKs are not required.
package provider

import (
	"context"
	"encoding/json"
	"io"
	"time"
)

// Capabilities is what one model can do. Requests using a capability the
// model lacks fail fast, since downgrading is the prompt pipeline's job.
type Capabilities struct {
	Streaming        bool
	NativeTools      bool
	StructuredOutput bool
	Vision           bool
	PDF              bool
	Thinking         bool
	FileUpload       bool
	PromptCaching    bool
	ContextWindow    int
	MaxOutputTokens  int
}

// Source is a readable file to send to a provider.
type Source interface {
	Open(ctx context.Context) (io.ReadCloser, error)
	Size() int64
	MediaType() string
}

// Part is one piece of a message. The sealed method keeps the set closed.
type Part interface {
	part()
}

// TextPart is plain or markdown text.
type TextPart struct{ Text string }

// ImagePart is an image, either from a Source or already uploaded (RemoteID).
type ImagePart struct {
	Source   Source
	RemoteID string // set when the file was uploaded to this provider
}

// FilePart is any other file, same remote/source split as ImagePart.
type FilePart struct {
	Name     string
	Source   Source
	RemoteID string
}

// ThinkingPart is a reasoning trace, stored for display and dropped on a
// provider switch unless the provider requires it.
type ThinkingPart struct {
	Text      string
	Signature string
}

// ToolCallPart is a request to run a tool. It is complete only at PartDone;
// argument fragments are exposed as deltas for UIs and are never executed.
type ToolCallPart struct {
	CallID    string
	Name      string
	Arguments json.RawMessage
}

// ToolResultPart is the output of a tool call.
type ToolResultPart struct {
	CallID  string
	Output  json.RawMessage
	IsError bool
}

func (TextPart) part()       {}
func (ImagePart) part()      {}
func (FilePart) part()       {}
func (ThinkingPart) part()   {}
func (ToolCallPart) part()   {}
func (ToolResultPart) part() {}

// Role is the speaker of a message.
type Role int

const (
	UserRole Role = iota
	AssistantRole
	ToolRole
)

// Message is one turn of the transcript as the provider sees it.
type Message struct {
	Role  Role
	Parts []Part
}

// ToolSpec advertises a callable tool to the model.
type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// Request is what the pipeline hands the adapter.
type Request struct {
	Model           string
	System          []Part
	Messages        []Message
	Tools           []ToolSpec
	OutputSchema    json.RawMessage
	MaxOutputTokens int
	Temperature     *float64
	Stop            []string
	Metadata        map[string]string
}

// Usage is what a call consumed. Estimated is set when the vendor reported
// no usage and the tokenizer's estimate is stored instead.
type Usage struct {
	InputTokens     int
	OutputTokens    int
	CachedTokens    int
	ReasoningTokens int
	Estimated       bool
}

// FinishReason is why the model stopped.
type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishContentFilter FinishReason = "content_filter"
	FinishError         FinishReason = "error"
)

// Response is the non-streaming result of a Generate call.
type Response struct {
	Message           Message
	Usage             Usage
	Finish            FinishReason
	Model             string
	ProviderRequestID string
}

// EventKind is the sort of streaming event.
type EventKind int

const (
	PartStarted EventKind = iota
	PartDelta
	PartDone
	UsageReported
	Finished
)

// Event is one streamed update. Delta carries a text or argument fragment;
// Part carries the complete part on PartDone.
type Event struct {
	Kind   EventKind
	Index  int // part index within the message
	Delta  string
	Part   Part
	Usage  *Usage
	Finish FinishReason
}

// Stream is a running Generate. Recv returns io.EOF after Finished, and
// Close is safe at any time. Cancelling the context closes the HTTP body.
type Stream interface {
	Recv() (Event, error)
	Close() error
}

// Pricing is a vendor's list price, in micro-units of the vendor's currency
// (usually USD) per million tokens.
type Pricing struct {
	Currency            string
	InputMicrosPerMTok  int64
	OutputMicrosPerMTok int64
	CachedMicrosPerMTok int64
}

// ModelInfo describes one model.
type ModelInfo struct {
	ID           string
	Capabilities Capabilities
	Pricing      Pricing
}

// RemoteFile is a copy of a file the provider now holds.
type RemoteFile struct {
	ID        string
	ExpiresAt time.Time // zero when it does not expire
}

// Provider is the vendor-neutral model interface.
type Provider interface {
	Name() string
	Models(ctx context.Context) ([]ModelInfo, error)
	Capabilities(model string) (Capabilities, error)
	Generate(ctx context.Context, req Request) (Response, error)
	Stream(ctx context.Context, req Request) (Stream, error)
	UploadFile(ctx context.Context, src Source, name string) (RemoteFile, error)
}

// CredentialSource yields a provider's API key. Keys are never stored in the
// store and never logged.
type CredentialSource interface {
	Credential(ctx context.Context, providerName string) (string, error)
}


// @note #provider-opaque-state-lost issue status=open priority=P0 tags=#provider,#gemini,#architecture : Opaque provider state is lost at every port boundary
// @see #localuser-no-scopes
//
// Modern providers attach opaque, provider-generated state to parts and
// require it echoed back verbatim on the next request:
//   - Gemini 3+: thoughtSignature on the first functionCall part of a step
//     (it is a *sibling* of functionCall, not a field inside it)
//   - OpenAI Responses API: reasoning.encrypted_content on reasoning items
//   - Anthropic: thinking-block signatures
//
// Gemini's symptom is a 400 on the *second* turn of any tool-calling
// exchange, never the first: INVALID_ARGUMENT "Function call is missing a
// thought_signature" naming the first functionCall of the step after the
// user message. Parallel calls carry the signature on only the first;
// sequential steps each carry their own; only the current turn (from the
// last user text message) is validated. Older turns without signatures are
// tolerated, so the bug only bites after a tool has run.
//
// The signature is dropped at four boundaries on the way back:
//   1. wirePart in provider/gemini/gemini.go has no thoughtSignature field,
//      so decodeParts and the stream's mapFrame cannot capture it.
//   2. provider.ToolCallPart has no opaque passthrough, so even a captured
//      value has nowhere to live.
//   3. domain.Block for tool_call stores only call_id/name/arguments, so
//      both parseBlocks (the content.ParseParts path used when a registry
//      is wired) and assistantBlocks (the static fallback) drop it before
//      persistence.
//   4. agent.ToolCall and pendingCall drop it, so a RiskConfirm call parked
//      for approval loses the signature across the checkpoint and 400s when
//      the run resumes.
//
// Proposed fix (architectural, not a per-vendor field): treat Block.Raw as
// "provider-native representation, replayed verbatim" and add a namespaced
// opaque map (ProviderMeta map[string]json.RawMessage on both Part and
// Block, matching Vercel's providerOptions model). The adapter is the only
// code that reads or writes its own namespace; every other layer
// round-trips it opaquely. New providers then need no core change.
//
// Escape hatch for already-committed unsigned turns:
// "skip_thought_signature_validator". Google documents it as a last resort
// that degrades quality; inject it only when the signature is genuinely
// unrecoverable, never over a real one, and only on the first functionCall
// of a step.
//
// Invariant to enforce with a golden test: every part crossing
// Part -> Block -> ToolCall -> pendingCall -> Part must round-trip its
// opaque payload byte-for-byte. The LangChain and Vercel bug histories are
// violations of exactly this invariant at one converter each.
