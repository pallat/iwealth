package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestGenerateContent(t *testing.T) {
	var got struct {
		Model    string                           `json:"model"`
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", 400)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"choices":[{"message":{"content":"สรุป"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer srv.Close()

	d := &DeepSeek{BaseURL: srv.URL, Key: "k", ModelName: "deepseek-v4-pro", HTTP: srv.Client()}
	req := &model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("facts", genai.RoleUser)},
		Config:   &genai.GenerateContentConfig{SystemInstruction: genai.NewContentFromText("be brief", genai.RoleUser)},
	}
	var text string
	for resp, err := range d.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatal(err)
		}
		text = resp.Content.Parts[0].Text
	}
	if text != "สรุป" {
		t.Fatalf("text=%q", text)
	}
	if got.Model != "deepseek-v4-pro" || len(got.Messages) != 2 ||
		got.Messages[0].Role != "system" || got.Messages[1].Role != "user" {
		t.Fatalf("request: %+v", got)
	}
}

func TestGenerateContentHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }))
	defer srv.Close()
	d := &DeepSeek{BaseURL: srv.URL, Key: "k", ModelName: "m", HTTP: srv.Client()}
	var gotErr error
	for _, err := range d.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		gotErr = err
	}
	if gotErr == nil {
		t.Fatal("expected error")
	}
}
