package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCapsWriterHTTPSubmitPollCompleted(t *testing.T) {
	dir := t.TempDir()
	audioPath := filepath.Join(dir, "微信语音.wav")
	wantAudio := []byte("RIFF-test-audio")
	if err := os.WriteFile(audioPath, wantAudio, 0o600); err != nil {
		t.Fatal(err)
	}

	var polls atomic.Int32
	var seenIdempotency string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transcriptions":
			body, _ := io.ReadAll(r.Body)
			if string(body) != string(wantAudio) {
				t.Errorf("upload body = %q, want %q", string(body), string(wantAudio))
			}
			if r.URL.Query().Get("language") != "zh" || r.URL.Query().Get("context") != "医学实验" {
				t.Errorf("query = %q", r.URL.RawQuery)
			}
			if got := r.Header.Get("X-Filename"); got != filepath.Base(audioPath) {
				t.Errorf("X-Filename = %q", got)
			}
			seenIdempotency = r.Header.Get("Idempotency-Key")
			if !strings.HasPrefix(seenIdempotency, "wechat-cli-") {
				t.Errorf("Idempotency-Key = %q", seenIdempotency)
			}
			if r.Header.Get("Authorization") != "Bearer test-bearer" {
				t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			}
			if r.Header.Get("CF-Access-Client-Id") != "cf-id" || r.Header.Get("CF-Access-Client-Secret") != "cf-secret" {
				t.Errorf("Cloudflare Access headers missing")
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"ok":true,"id":"tr_test","status":"queued"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/transcriptions/tr_test":
			if polls.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"ok":true,"id":"tr_test","status":"running"}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"id":"tr_test","status":"completed","result":{"text":"老师说明天下午三点开会"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := capsWriterHTTPConfig{
		Enabled:              true,
		BaseURL:              server.URL,
		BearerToken:          "test-bearer",
		CFAccessClientID:     "cf-id",
		CFAccessClientSecret: "cf-secret",
		Language:             "zh",
		Context:              "医学实验",
		PollInterval:         time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := runCapsWriterHTTPVoiceASRWithClient(ctx, server.Client(), cfg, audioPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "老师说明天下午三点开会" || got.Engine != "capswriter-http" || got.JobID != "tr_test" || got.Endpoint != server.URL || got.Language != "zh" {
		t.Fatalf("result = %#v", got)
	}
	if polls.Load() != 2 {
		t.Fatalf("polls = %d, want 2", polls.Load())
	}
	if seenIdempotency == "" {
		t.Fatal("missing idempotency key")
	}
}

func TestCapsWriterHTTPIdempotencyKeyStableAndSemantic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voice.wav")
	if err := os.WriteFile(path, []byte("same-audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := func(language, prompt string) string {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		got, err := capsWriterHTTPIdempotencyKey(f, language, prompt)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if a, b := key("zh", "ctx"), key("zh", "ctx"); a != b {
		t.Fatalf("same request produced different keys: %q != %q", a, b)
	}
	if a, b := key("zh", "ctx"), key("en", "ctx"); a == b {
		t.Fatalf("language must affect idempotency key: %q", a)
	}
	if a, b := key("zh", "ctx"), key("zh", "other"); a == b {
		t.Fatalf("context must affect idempotency key: %q", a)
	}
}

func TestCapsWriterHTTPFailedJobReturnsError(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "voice.wav")
	_ = os.WriteFile(audioPath, []byte("audio"), 0o600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"ok":true,"id":"tr_fail","status":"queued"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":false,"id":"tr_fail","status":"failed","error":{"code":"asr_failed","message":"model unavailable","retryable":true}}`)
	}))
	defer server.Close()
	cfg := capsWriterHTTPConfig{Enabled: true, BaseURL: server.URL, Language: "zh", PollInterval: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := runCapsWriterHTTPVoiceASRWithClient(ctx, server.Client(), cfg, audioPath)
	if err == nil || !strings.Contains(err.Error(), "model unavailable") {
		t.Fatalf("err = %v, want model unavailable", err)
	}
}

func TestCapsWriterHTTPPollingHonorsContextTimeout(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "voice.wav")
	_ = os.WriteFile(audioPath, []byte("audio"), 0o600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
		}
		_, _ = io.WriteString(w, `{"ok":true,"id":"tr_wait","status":"running"}`)
	}))
	defer server.Close()
	cfg := capsWriterHTTPConfig{Enabled: true, BaseURL: server.URL, Language: "zh", PollInterval: 5 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := runCapsWriterHTTPVoiceASRWithClient(ctx, server.Client(), cfg, audioPath)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
}

func TestCapsWriterHTTPHealthUsesAuthHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" || r.Header.Get("Authorization") != "Bearer token" {
			http.Error(w, "bad request", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()
	cfg := capsWriterHTTPConfig{Enabled: true, BaseURL: server.URL, BearerToken: "token"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := capsWriterHTTPHealth(ctx, server.Client(), cfg); err != nil {
		t.Fatal(err)
	}
}
