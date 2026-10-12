package advisor

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

type fakeLLM struct {
	mu        sync.Mutex
	gotFacts  string
	gotSystem string
	reqFacts  []string
	err       error
	reply     string
}

func (f *fakeLLM) Name() string { return "fake" }
func (f *fakeLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		f.mu.Lock()
		var facts string
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				facts += p.Text
			}
		}
		f.gotFacts = facts
		f.reqFacts = append(f.reqFacts, facts)
		if req.Config != nil && req.Config.SystemInstruction != nil {
			f.gotSystem = req.Config.SystemInstruction.Parts[0].Text
		}
		f.mu.Unlock()
		if f.err != nil {
			yield(nil, f.err)
			return
		}
		reply := f.reply
		if reply == "" {
			reply = "ตลาดหุ้นลงทั้งแถบ"
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText(reply, genai.RoleModel), TurnComplete: true}, nil)
	}
}

func TestSummarize(t *testing.T) {
	f := &fakeLLM{}
	a, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Summarize(context.Background(), "FUND A drop 12%")
	if err != nil || out != "ตลาดหุ้นลงทั้งแถบ" {
		t.Fatalf("%q %v", out, err)
	}
	if !strings.Contains(f.gotFacts, "FUND A drop 12%") {
		t.Fatalf("facts not forwarded: %q", f.gotFacts)
	}
	if !strings.Contains(f.gotSystem, "unknown") {
		t.Fatalf("system prompt must forbid guessing: %q", f.gotSystem)
	}
}

func TestSummarizeLLMError(t *testing.T) {
	want := errors.New("boom")
	a, err := New(&fakeLLM{err: want})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Summarize(context.Background(), "facts")
	if err == nil || !errors.Is(err, want) || out != "" {
		t.Fatalf("want boom error, got %q %v", out, err)
	}
}

func TestSummarizeCanceled(t *testing.T) {
	a, err := New(&fakeLLM{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := a.Summarize(ctx, "facts"); err == nil {
		t.Fatalf("expected error on canceled ctx, got %q", out)
	}
}

func TestSummarizeFreshSessions(t *testing.T) {
	f := &fakeLLM{}
	a, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, facts := range []string{"FIRST-FACTS", "SECOND-FACTS"} {
		if _, err := a.Summarize(context.Background(), facts); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.reqFacts) != 2 {
		t.Fatalf("want 2 requests, got %d", len(f.reqFacts))
	}
	if strings.Contains(f.reqFacts[1], "FIRST-FACTS") || !strings.Contains(f.reqFacts[1], "SECOND-FACTS") {
		t.Fatalf("session leaked: %q", f.reqFacts[1])
	}
}

func TestSummarizeConcurrent(t *testing.T) {
	a, err := New(&fakeLLM{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Summarize(context.Background(), "x"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
