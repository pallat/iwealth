package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

type DeepSeek struct {
	BaseURL, Key, ModelName string
	HTTP                    *http.Client
}

func NewDeepSeek(key string) *DeepSeek {
	return &DeepSeek{
		BaseURL: "https://api.deepseek.com", Key: key, ModelName: "deepseek-v4-pro",
		HTTP: &http.Client{Timeout: 120 * time.Second},
	}
}

func (d *DeepSeek) Name() string { return d.ModelName }

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func text(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range c.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func (d *DeepSeek) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		var msgs []chatMsg
		if req.Config != nil && req.Config.SystemInstruction != nil {
			msgs = append(msgs, chatMsg{"system", text(req.Config.SystemInstruction)})
		}
		for _, c := range req.Contents {
			role := "user"
			if c.Role == genai.RoleModel {
				role = "assistant"
			}
			msgs = append(msgs, chatMsg{role, text(c)})
		}
		body, _ := json.Marshal(map[string]any{"model": d.ModelName, "messages": msgs})
		hr, err := http.NewRequestWithContext(ctx, http.MethodPost, d.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			yield(nil, err)
			return
		}
		hr.Header.Set("Authorization", "Bearer "+d.Key)
		hr.Header.Set("Content-Type", "application/json")
		res, err := d.HTTP.Do(hr)
		if err != nil {
			yield(nil, err)
			return
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		if res.StatusCode/100 != 2 {
			yield(nil, fmt.Errorf("deepseek: status %d: %s", res.StatusCode, truncate(string(raw), 200)))
			return
		}
		var out struct {
			Choices []struct {
				Message struct{ Content string } `json:"message"`
			} `json:"choices"`
			Usage struct {
				Prompt     int32 `json:"prompt_tokens"`
				Completion int32 `json:"completion_tokens"`
				Total      int32 `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
			yield(nil, fmt.Errorf("deepseek: bad response: %v", err))
			return
		}
		yield(&model.LLMResponse{
			Content:       genai.NewContentFromText(out.Choices[0].Message.Content, genai.RoleModel),
			TurnComplete:  true,
			FinishReason:  genai.FinishReasonStop,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: out.Usage.Prompt, CandidatesTokenCount: out.Usage.Completion, TotalTokenCount: out.Usage.Total},
		}, nil)
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
