// Package telegram is a minimal Telegram Bot API client (send + long-poll commands).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxRunes = 4000

// Bot talks to the Telegram Bot API. The token is part of every request URL,
// so errors are sanitized and never include it.
type Bot struct {
	Token   string
	ChatID  int64
	BaseURL string
	HTTP    *http.Client

	// OnError, if set, receives getUpdates and reply errors (already token-free).
	// Repeats of the same poll error are reported once, then at most once per
	// errRepeat, so a long outage cannot flood the log.
	OnError func(error)

	now     func() time.Time // test hook; time.Now when nil
	idle    time.Duration    // pause between successful polls (0 in production; long-poll already blocks)
	backoff time.Duration    // wait after a getUpdates error
}

// New returns a Bot for the public Telegram API.
func New(token string, chatID int64) *Bot {
	return &Bot{
		Token:   token,
		ChatID:  chatID,
		BaseURL: "https://api.telegram.org",
		HTTP:    &http.Client{Timeout: 45 * time.Second},
		backoff: 5 * time.Second,
	}
}

type apiResp struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

// errRepeat is how often an unchanged poll error is reported again.
const errRepeat = time.Minute

func (b *Bot) report(err error) {
	if b.OnError != nil && err != nil {
		b.OnError(err)
	}
}

func (b *Bot) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *Bot) client() *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return &http.Client{Timeout: 45 * time.Second}
}

// do performs the request and returns the decoded result, never leaking the token.
func (b *Bot) do(req *http.Request, method string) (json.RawMessage, error) {
	resp, err := b.client().Do(req)
	if err != nil {
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("telegram %s: %s", method, b.scrub(err.Error()))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("telegram %s: read body: %s", method, b.scrub(err.Error()))
	}
	var r apiResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("telegram %s: http %d: invalid response", method, resp.StatusCode)
	}
	if !r.OK {
		return nil, fmt.Errorf("telegram %s: %s", method, b.scrub(r.Description))
	}
	return r.Result, nil
}

func (b *Bot) scrub(s string) string {
	if b.Token == "" {
		return s
	}
	return strings.ReplaceAll(s, b.Token, "<token>")
}

func (b *Bot) endpoint(method string) string {
	return b.BaseURL + "/bot" + b.Token + "/" + method
}

func (b *Bot) sendOne(ctx context.Context, chatID int64, text string) error {
	payload, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint("sendMessage"), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("telegram sendMessage: build request")
	}
	req.Header.Set("Content-Type", "application/json")
	_, err = b.do(req, "sendMessage")
	return err
}

// Send posts text to the configured chat as plain text, splitting at 4000 runes.
func (b *Bot) Send(ctx context.Context, text string) error {
	return b.sendTo(ctx, b.ChatID, text)
}

func (b *Bot) sendTo(ctx context.Context, chatID int64, text string) error {
	r := []rune(text)
	if len(r) == 0 {
		return nil
	}
	for len(r) > 0 {
		n := min(len(r), maxRunes)
		if err := b.sendOne(ctx, chatID, string(r[:n])); err != nil {
			return err
		}
		r = r[n:]
	}
	return nil
}

func (b *Bot) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	u := b.endpoint("getUpdates") + "?timeout=30&offset=" + strconv.FormatInt(offset, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("telegram getUpdates: build request")
	}
	raw, err := b.do(req, "getUpdates")
	if err != nil {
		return nil, err
	}
	var ups []update
	if err := json.Unmarshal(raw, &ups); err != nil {
		return nil, fmt.Errorf("telegram getUpdates: invalid result")
	}
	return ups, nil
}

func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func safeHandle(ctx context.Context, h func(context.Context, string) string, text string) (out string) {
	defer func() {
		if recover() != nil {
			out = ""
		}
	}()
	return h(ctx, text)
}

// Poll long-polls getUpdates and calls handle for text messages from ChatID only,
// replying with the returned string when non-empty. It returns nil once ctx is done.
func (b *Bot) Poll(ctx context.Context, handle func(ctx context.Context, text string) string) error {
	var offset int64
	var lastErr string
	var lastAt time.Time
	for ctx.Err() == nil {
		ups, err := b.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() == nil {
				if msg := err.Error(); msg != lastErr || b.clock().Sub(lastAt) >= errRepeat {
					lastErr, lastAt = msg, b.clock()
					b.report(err)
				}
			}
			sleep(ctx, b.backoff)
			continue
		}
		lastErr = ""
		for _, u := range ups {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message == nil || u.Message.Chat.ID != b.ChatID || u.Message.Text == "" {
				continue
			}
			if reply := safeHandle(ctx, handle, u.Message.Text); reply != "" {
				if err := b.sendTo(ctx, b.ChatID, reply); err != nil && ctx.Err() == nil {
					b.report(err)
				}
			}
		}
		sleep(ctx, b.idle)
	}
	return nil
}

// ParsePaid parses "/paid <fund> <amount>" (also "/paid@BotName ...").
func ParsePaid(text string) (fund string, amount float64, err error) {
	f := strings.Fields(text)
	if len(f) != 3 {
		return "", 0, errors.New("usage: /paid <fund> <amount>")
	}
	cmd := f[0]
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	if cmd != "/paid" {
		return "", 0, errors.New("usage: /paid <fund> <amount>")
	}
	amount, err = strconv.ParseFloat(f[2], 64)
	if err != nil || math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 {
		return "", 0, errors.New("amount must be a positive number")
	}
	return f[1], amount, nil
}

// ParseDeduct parses "/deduct <key> <amount>" (also "/deduct@BotName ..."). The
// amount is a year-to-date total; 0 clears the entry.
func ParseDeduct(text string) (key string, amount float64, err error) {
	f := strings.Fields(text)
	cmd := ""
	if len(f) > 0 {
		cmd = f[0]
		if i := strings.IndexByte(cmd, '@'); i >= 0 {
			cmd = cmd[:i]
		}
	}
	if len(f) != 3 || cmd != "/deduct" {
		return "", 0, errors.New("usage: /deduct <key> <amount>")
	}
	amount, err = strconv.ParseFloat(f[2], 64)
	if err != nil || math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
		return "", 0, errors.New("amount must be a number, 0 or greater")
	}
	return strings.ToLower(f[1]), amount, nil
}
