package main

// 本文件只负责把“一个已经可读的本地音频文件”交给 CapsWriterGateway。
//
// 这里刻意不放任何微信数据库、SILK 解码或 agent payload 逻辑：
//
//   微信消息 -> 找到 VoiceInfo -> SILK 解码成 WAV
//                              |
//                              v
//                    本文件：HTTP Job Client
//                              |
//                              v
//             CapsWriterGateway :6017 -> CapsWriter :6016
//
// 这样 CapsWriter Offline 仍然只负责 ASR 内核与 mic > file 抢占调度；HTTP、
// Cloudflare、微信等业务边界全部留在 Gateway / WeChatCLI 外围，后续升级
// CapsWriter upstream 时不会把这些业务协议重新耦合进模型 Server。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultCapsWriterHTTPURL          = "http://127.0.0.1:6017"
	defaultCapsWriterHTTPPollInterval = 400 * time.Millisecond
	maxCapsWriterHTTPResponseBytes    = 4 << 20 // 4 MiB：job JSON 只应是小型控制面数据。
)

var errCapsWriterHTTPDisabled = errors.New("CapsWriter HTTP ASR is disabled")

// voiceASRResult 是 WeChatCLI 内部统一的 ASR 结果。
//
// Text 是最终给 Agent 的正文；其余字段只用于可观测性和 transcript cache。
// 传统 custom/faster-whisper 路径可以只填写 Text/Engine/Model；HTTP 路径还会
// 记录 JobID 与 Endpoint，方便将来排查“微信消息 -> Gateway job”的对应关系。
type voiceASRResult struct {
	Text     string
	Engine   string
	Model    string
	Language string
	JobID    string
	Endpoint string
}

type capsWriterHTTPConfig struct {
	Enabled              bool
	BaseURL              string
	BearerToken          string
	CFAccessClientID     string
	CFAccessClientSecret string
	Language             string
	Context              string
	PollInterval         time.Duration
}

// capsWriterHTTPConfigFromEnv 读取通用 Gateway 客户端配置。
//
// 默认直接使用本机 127.0.0.1:6017，因此正式安装无需再额外写一份 endpoint
// 配置。远端场景可以覆盖 URL，并可选携带 Gateway Bearer 或 Cloudflare Access
// Service Token。状态输出永远只显示“是否配置”，不会打印这些 secret。
func capsWriterHTTPConfigFromEnv() capsWriterHTTPConfig {
	enabled := true
	if raw := envFirst("WECHAT_CLI_ASR_HTTP_ENABLED", "WX_MCP_ASR_HTTP_ENABLED"); raw != "" {
		enabled = parseASREnabled(raw)
	}

	baseURL := firstNonEmpty(
		envFirst("WECHAT_CLI_ASR_HTTP_URL", "WX_MCP_ASR_HTTP_URL", "CAPSWRITER_GATEWAY_URL"),
		defaultCapsWriterHTTPURL,
	)
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.EqualFold(baseURL, "off") || strings.EqualFold(baseURL, "disabled") || strings.EqualFold(baseURL, "none") {
		enabled = false
		baseURL = defaultCapsWriterHTTPURL
	}

	poll := defaultCapsWriterHTTPPollInterval
	if raw := envFirst("WECHAT_CLI_ASR_HTTP_POLL_MS", "WX_MCP_ASR_HTTP_POLL_MS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 10 {
			poll = time.Duration(n) * time.Millisecond
		}
	}

	return capsWriterHTTPConfig{
		Enabled:              enabled,
		BaseURL:              baseURL,
		BearerToken:          envFirst("WECHAT_CLI_ASR_HTTP_BEARER_TOKEN", "WX_MCP_ASR_HTTP_BEARER_TOKEN", "CAPSWRITER_GATEWAY_TOKEN"),
		CFAccessClientID:     envFirst("WECHAT_CLI_ASR_HTTP_CF_ACCESS_CLIENT_ID", "WX_MCP_ASR_HTTP_CF_ACCESS_CLIENT_ID"),
		CFAccessClientSecret: envFirst("WECHAT_CLI_ASR_HTTP_CF_ACCESS_CLIENT_SECRET", "WX_MCP_ASR_HTTP_CF_ACCESS_CLIENT_SECRET"),
		Language: firstNonEmpty(
			envFirst("WECHAT_CLI_ASR_HTTP_LANGUAGE", "WX_MCP_ASR_HTTP_LANGUAGE"),
			envFirst("WECHAT_CLI_VOICE_LANGUAGE", "WX_MCP_VOICE_LANGUAGE"),
			"zh",
		),
		Context:      envFirst("WECHAT_CLI_ASR_HTTP_CONTEXT", "WX_MCP_ASR_HTTP_CONTEXT"),
		PollInterval: poll,
	}
}

func parseASREnabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return true
	}
}

type capsWriterHTTPJob struct {
	OK     bool                    `json:"ok"`
	ID     string                  `json:"id"`
	Status string                  `json:"status"`
	Result *capsWriterHTTPJobResult `json:"result"`
	Error  *capsWriterHTTPJobError  `json:"error"`
}

type capsWriterHTTPJobResult struct {
	Text string `json:"text"`
}

type capsWriterHTTPJobError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// runCapsWriterHTTPVoiceASR 是生产入口。总时限沿用 WeChatCLI 现有
// voiceCommandTimeout()，因此不会因为 Gateway 或网络异常让一次聊天读取永久挂住。
func runCapsWriterHTTPVoiceASR(audioPath string) (voiceASRResult, error) {
	cfg := capsWriterHTTPConfigFromEnv()
	if !cfg.Enabled {
		return voiceASRResult{}, errCapsWriterHTTPDisabled
	}
	ctx, cancel := context.WithTimeout(context.Background(), voiceCommandTimeout())
	defer cancel()
	return runCapsWriterHTTPVoiceASRWithClient(ctx, http.DefaultClient, cfg, audioPath)
}

// runCapsWriterHTTPVoiceASRWithClient 实现“提交 -> 轮询同一个 job 资源 -> 完成”。
// client 作为参数注入，既便于 httptest，也避免在业务函数里偷偷改全局 HTTP 状态。
func runCapsWriterHTTPVoiceASRWithClient(ctx context.Context, client *http.Client, cfg capsWriterHTTPConfig, audioPath string) (voiceASRResult, error) {
	if !cfg.Enabled {
		return voiceASRResult{}, errCapsWriterHTTPDisabled
	}
	if client == nil {
		client = http.DefaultClient
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return voiceASRResult{}, fmt.Errorf("CapsWriter HTTP base URL is empty")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultCapsWriterHTTPPollInterval
	}

	job, err := submitCapsWriterHTTPJob(ctx, client, cfg, audioPath)
	if err != nil {
		return voiceASRResult{}, err
	}
	for {
		if result, done, err := capsWriterHTTPJobResultValue(cfg, job); done || err != nil {
			return result, err
		}

		timer := time.NewTimer(cfg.PollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return voiceASRResult{}, fmt.Errorf("CapsWriter HTTP transcription timed out: %w", ctx.Err())
		case <-timer.C:
		}

		job, err = getCapsWriterHTTPJob(ctx, client, cfg, job.ID)
		if err != nil {
			return voiceASRResult{}, err
		}
	}
}

func submitCapsWriterHTTPJob(ctx context.Context, client *http.Client, cfg capsWriterHTTPConfig, audioPath string) (capsWriterHTTPJob, error) {
	file, err := os.Open(audioPath)
	if err != nil {
		return capsWriterHTTPJob{}, fmt.Errorf("open ASR audio: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return capsWriterHTTPJob{}, fmt.Errorf("stat ASR audio: %w", err)
	}
	if stat.IsDir() || stat.Size() <= 0 {
		return capsWriterHTTPJob{}, fmt.Errorf("ASR audio is empty or not a regular file")
	}

	idempotencyKey, err := capsWriterHTTPIdempotencyKey(file, cfg.Language, cfg.Context)
	if err != nil {
		return capsWriterHTTPJob{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return capsWriterHTTPJob{}, fmt.Errorf("rewind ASR audio: %w", err)
	}

	endpoint, err := capsWriterHTTPTranscriptionsURL(cfg)
	if err != nil {
		return capsWriterHTTPJob{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, file)
	if err != nil {
		return capsWriterHTTPJob{}, fmt.Errorf("build CapsWriter HTTP submit request: %w", err)
	}
	req.ContentLength = stat.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Filename", filepath.Base(audioPath))
	req.Header.Set("Idempotency-Key", idempotencyKey)
	applyCapsWriterHTTPAuthHeaders(req.Header, cfg)

	return doCapsWriterHTTPJobRequest(client, req, http.StatusOK, http.StatusAccepted)
}

func getCapsWriterHTTPJob(ctx context.Context, client *http.Client, cfg capsWriterHTTPConfig, jobID string) (capsWriterHTTPJob, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return capsWriterHTTPJob{}, fmt.Errorf("CapsWriter HTTP returned an empty job id")
	}
	endpoint := cfg.BaseURL + "/v1/transcriptions/" + url.PathEscape(jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return capsWriterHTTPJob{}, fmt.Errorf("build CapsWriter HTTP poll request: %w", err)
	}
	applyCapsWriterHTTPAuthHeaders(req.Header, cfg)
	return doCapsWriterHTTPJobRequest(client, req, http.StatusOK)
}

func capsWriterHTTPTranscriptionsURL(cfg capsWriterHTTPConfig) (string, error) {
	u, err := url.Parse(cfg.BaseURL + "/v1/transcriptions")
	if err != nil {
		return "", fmt.Errorf("parse CapsWriter HTTP URL: %w", err)
	}
	q := u.Query()
	if cfg.Language != "" {
		q.Set("language", cfg.Language)
	}
	if cfg.Context != "" {
		q.Set("context", cfg.Context)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func capsWriterHTTPIdempotencyKey(file *os.File, language, promptContext string) (string, error) {
	if file == nil {
		return "", fmt.Errorf("ASR audio file is nil")
	}
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", fmt.Errorf("hash ASR audio: %w", err)
	}
	_, _ = io.WriteString(h, "\x00language=")
	_, _ = io.WriteString(h, language)
	_, _ = io.WriteString(h, "\x00context=")
	_, _ = io.WriteString(h, promptContext)
	// 完整 SHA-256 很便宜，也避免只截短 hash 后让两个不同微信语音共享同一 job。
	return "wechat-cli-" + hex.EncodeToString(h.Sum(nil)), nil
}

func applyCapsWriterHTTPAuthHeaders(headers http.Header, cfg capsWriterHTTPConfig) {
	if token := strings.TrimSpace(cfg.BearerToken); token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	if value := strings.TrimSpace(cfg.CFAccessClientID); value != "" {
		headers.Set("CF-Access-Client-Id", value)
	}
	if value := strings.TrimSpace(cfg.CFAccessClientSecret); value != "" {
		headers.Set("CF-Access-Client-Secret", value)
	}
}

func doCapsWriterHTTPJobRequest(client *http.Client, req *http.Request, allowedStatus ...int) (capsWriterHTTPJob, error) {
	resp, err := client.Do(req)
	if err != nil {
		return capsWriterHTTPJob{}, fmt.Errorf("CapsWriter HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCapsWriterHTTPResponseBytes+1))
	if err != nil {
		return capsWriterHTTPJob{}, fmt.Errorf("read CapsWriter HTTP response: %w", err)
	}
	if len(body) > maxCapsWriterHTTPResponseBytes {
		return capsWriterHTTPJob{}, fmt.Errorf("CapsWriter HTTP response exceeds %d bytes", maxCapsWriterHTTPResponseBytes)
	}
	allowed := false
	for _, status := range allowedStatus {
		if resp.StatusCode == status {
			allowed = true
			break
		}
	}
	if !allowed {
		return capsWriterHTTPJob{}, fmt.Errorf("CapsWriter HTTP returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var job capsWriterHTTPJob
	if err := json.Unmarshal(body, &job); err != nil {
		return capsWriterHTTPJob{}, fmt.Errorf("decode CapsWriter HTTP job: %w", err)
	}
	if strings.TrimSpace(job.ID) == "" {
		return capsWriterHTTPJob{}, fmt.Errorf("CapsWriter HTTP response is missing job id")
	}
	return job, nil
}

func capsWriterHTTPJobResultValue(cfg capsWriterHTTPConfig, job capsWriterHTTPJob) (voiceASRResult, bool, error) {
	switch strings.ToLower(strings.TrimSpace(job.Status)) {
	case "queued", "running":
		return voiceASRResult{}, false, nil
	case "completed":
		text := ""
		if job.Result != nil {
			text = strings.TrimSpace(job.Result.Text)
		}
		return voiceASRResult{
			Text:     text,
			Engine:   "capswriter-http",
			Language: cfg.Language,
			JobID:    job.ID,
			Endpoint: cfg.BaseURL,
		}, true, nil
	case "failed":
		message := "transcription failed"
		if job.Error != nil {
			message = firstNonEmpty(strings.TrimSpace(job.Error.Message), strings.TrimSpace(job.Error.Code), message)
		}
		return voiceASRResult{}, true, fmt.Errorf("CapsWriter HTTP job %s failed: %s", job.ID, message)
	default:
		return voiceASRResult{}, true, fmt.Errorf("CapsWriter HTTP job %s has unknown status %q", job.ID, job.Status)
	}
}

// capsWriterHTTPHealth 用很短的请求验证 Gateway 控制面是否可达。
// 该函数只供 `asr status` 诊断使用，不参与真实转写决策；即使 health 瞬时失败，
// voice 路径仍会实际尝试 Job API，然后按既有 fallback 顺序降级。
func capsWriterHTTPHealth(ctx context.Context, client *http.Client, cfg capsWriterHTTPConfig) error {
	if !cfg.Enabled {
		return errCapsWriterHTTPDisabled
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	applyCapsWriterHTTPAuthHeaders(req.Header, cfg)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("CapsWriter HTTP health returned %s", resp.Status)
	}
	return nil
}
