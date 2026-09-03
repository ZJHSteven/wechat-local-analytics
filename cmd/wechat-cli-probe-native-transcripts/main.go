// Command wechat-cli-probe-native-transcripts 是一个仅供开发/回归测试使用的
// 真库探针：它直接扫描当前 wechat-cli 配置指向账号的 message_*.db，寻找
// 微信客户端已经写回 packed_info_data 的“原生语音转文字”缓存。
//
// 这个命令不触发任何 ASR，也不修改微信数据库；它的唯一目的，是把
// internal/wxparse.NativeVoiceTranscript 在真实 WeChat 4.x 数据上做端到端验收。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/r266-tech/wechat-cli/internal/config"
	"github.com/r266-tech/wechat-cli/internal/wcdb"
	"github.com/r266-tech/wechat-cli/internal/wxparse"
)

type hit struct {
	DB         string `json:"db"`
	Table      string `json:"table"`
	LocalID    int64  `json:"local_id"`
	ServerID   int64  `json:"server_id"`
	CreateTime int64  `json:"create_time"`
	Text       string `json:"text"`
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	if cfg.DBRoot == "" || len(cfg.Keys) == 0 {
		fatal(fmt.Errorf("wechat-cli config is not ready: db_root/keys missing"))
	}

	lib, err := resolveWCDBLibrary()
	if err != nil {
		fatal(err)
	}
	if err := wcdb.Bootstrap(lib); err != nil {
		fatal(err)
	}

	files, err := filepath.Glob(filepath.Join(cfg.DBRoot, "db_storage", "message", "message_*.db"))
	if err != nil {
		fatal(err)
	}
	files = numberedMessageShards(files)

	var hits []hit
	for _, path := range files {
		db, err := wcdb.OpenWithKeyMap(path, cfg.Keys)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", filepath.Base(path), err)
			continue
		}
		rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Msg_%'")
		if err != nil {
			db.Close()
			fmt.Fprintf(os.Stderr, "skip tables %s: %v\n", filepath.Base(path), err)
			continue
		}
		for _, row := range rows {
			table, _ := row["name"].(string)
			if !strings.HasPrefix(table, "Msg_") {
				continue
			}
			q := fmt.Sprintf(
				`SELECT local_id, server_id, create_time, packed_info_data FROM "%s" `+
					`WHERE (local_type & 4294967295)=34 AND server_id>0 `+
					`AND packed_info_data IS NOT NULL AND length(packed_info_data)>0`,
				strings.ReplaceAll(table, `"`, `""`),
			)
			voiceRows, err := db.Query(q)
			if err != nil {
				continue
			}
			for _, vr := range voiceRows {
				packed, _ := vr["packed_info_data"].([]byte)
				text := wxparse.NativeVoiceTranscript(packed)
				if text == "" {
					continue
				}
				hits = append(hits, hit{
					DB:         filepath.Base(path),
					Table:      table,
					LocalID:    int64Value(vr["local_id"]),
					ServerID:   int64Value(vr["server_id"]),
					CreateTime: int64Value(vr["create_time"]),
					Text:       text,
				})
				if len(hits) >= 20 {
					break
				}
			}
			if len(hits) >= 20 {
				break
			}
		}
		db.Close()
		if len(hits) >= 20 {
			break
		}
	}

	out := map[string]any{"ok": true, "hit_count": len(hits), "hits": hits}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fatal(err)
	}
}

func numberedMessageShards(paths []string) []string {
	var out []string
	for _, p := range paths {
		base := strings.TrimSuffix(filepath.Base(p), ".db")
		if !strings.HasPrefix(base, "message_") {
			continue
		}
		suffix := strings.TrimPrefix(base, "message_")
		if suffix == "" {
			continue
		}
		allDigits := true
		for _, r := range suffix {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func resolveWCDBLibrary() (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("WECHAT_CLI_WCDB_LIB")); explicit != "" {
		return explicit, nil
	}
	exe, _ := os.Executable()
	for _, p := range []string{
		filepath.Join(filepath.Dir(exe), "libWCDB.dll"),
		filepath.Join(".", "libWCDB.dll"),
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return filepath.Abs(p)
		}
	}
	return "", fmt.Errorf("libWCDB.dll not found; set WECHAT_CLI_WCDB_LIB")
}

func int64Value(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
