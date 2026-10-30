package workspacetest

import (
	"context"
	"io"
	"sync"

	"github.com/asaidimu/workspace/provider"
)

// ScriptedProvider is a provider double that replays a fixed script of
// replies and records every request it was handed. No test needs a network.
type ScriptedProvider struct {
	mu       sync.Mutex
	name     string
	script   []provider.Response
	requests []provider.Request
}

// NewScriptedProvider returns a provider that serves the replies queued on
// it, in order, and fails once the script runs out.
func NewScriptedProvider(name string) *ScriptedProvider {
	return &ScriptedProvider{name: name}
}

// ReplyText queues a plain-text assistant reply.
func (p *ScriptedProvider) ReplyText(text string) *ScriptedProvider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.script = append(p.script, provider.Response{
		Message: provider.Message{
			Role:  provider.AssistantRole,
			Parts: []provider.Part{provider.TextPart{Text: text}},
		},
		Finish: provider.FinishStop,
		Model:  p.name,
	})
	return p
}

// ReplyToolCalls queues an assistant reply that asks for tools. A complete
// ToolCallPart arrives only at PartDone, so the delta fragments a streaming
// adapter would emit are not simulated here.
func (p *ScriptedProvider) ReplyToolCalls(calls ...provider.ToolCallPart) *ScriptedProvider {
	p.mu.Lock()
	defer p.mu.Unlock()
	parts := make([]provider.Part, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, c)
	}
	p.script = append(p.script, provider.Response{
		Message: provider.Message{Role: provider.AssistantRole, Parts: parts},
		Finish:  provider.FinishToolCalls,
		Model:   p.name,
	})
	return p
}

// FailWith queues an error for the next call.
func (p *ScriptedProvider) FailWith(err error) *ScriptedProvider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.script = append(p.script, provider.Response{Finish: provider.FinishError, Model: p.name})
	return p
}

// Requests returns every request the provider was asked to serve, in order.
func (p *ScriptedProvider) Requests() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]provider.Request, len(p.requests))
	copy(out, p.requests)
	return out
}

// The provider.Provider implementation. Streaming, upload and capability
// queries are stubs until an adapter exists to exercise them; Generate is
// the one path the agent loop needs for a scripted test.

func (p *ScriptedProvider) Name() string { return p.name }

func (p *ScriptedProvider) Models(ctx context.Context) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{ID: p.name}}, nil
}

func (p *ScriptedProvider) Capabilities(model string) (provider.Capabilities, error) {
	return provider.Capabilities{ContextWindow: 200000, MaxOutputTokens: 8192}, nil
}

func (p *ScriptedProvider) Generate(ctx context.Context, req provider.Request) (provider.Response, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	if len(p.script) == 0 {
		p.mu.Unlock()
		return provider.Response{}, errScriptExhausted
	}
	resp := p.script[0]
	p.script = p.script[1:]
	p.mu.Unlock()
	return resp, nil
}

func (p *ScriptedProvider) Stream(ctx context.Context, req provider.Request) (provider.Stream, error) {
	resp, err := p.Generate(ctx, req)
	if err != nil {
		return nil, err
	}
	return &scriptedStream{resp: resp}, nil
}

func (p *ScriptedProvider) UploadFile(ctx context.Context, src provider.Source, name string) (provider.RemoteFile, error) {
	return provider.RemoteFile{}, errUnsupported
}

// scriptedStream replays one response as a stream: one PartDone per part,
// then Finished. Recv returns io.EOF after Finished, as the interface
// requires.
type scriptedStream struct {
	resp   provider.Response
	sent   int
	done   bool
	closed bool
}

func (s *scriptedStream) Recv() (provider.Event, error) {
	if s.done {
		return provider.Event{}, io.EOF
	}
	if s.sent < len(s.resp.Message.Parts) {
		part := s.resp.Message.Parts[s.sent]
		s.sent++
		return provider.Event{Kind: provider.PartDone, Index: s.sent - 1, Part: part}, nil
	}
	s.done = true
	return provider.Event{Kind: provider.Finished, Finish: s.resp.Finish}, nil
}

func (s *scriptedStream) Close() error {
	s.closed = true
	return nil
}

// The doubles' sentinel errors. They are package-local so a test can assert
// on them with errors.Is without the package exporting an error hierarchy.
type scriptedError string

func (e scriptedError) Error() string { return string(e) }

const (
	errScriptExhausted scriptedError = "workspacetest: scripted provider has no reply left"
	errUnsupported     scriptedError = "workspacetest: not supported by the scripted provider"
)
