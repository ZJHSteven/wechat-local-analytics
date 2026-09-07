package main

// 这个文件实现第二阶段的 canonical archive（规范化长期归档）。
//
// 设计目标不是“再复制一份微信目录”，而是把微信当前版本的私有数据库格式
// 转换为我们自己长期稳定的 SQLite schema。这样将来微信升级、密钥方案变化，
// 已经归档的数据仍然可以独立搜索和读取。
//
// 可靠性原则：
//   1. 枚举所有 message_N / biz_message_N shard 的全部 Msg_* 表，不依赖 SessionTable；
//   2. server_id 存在时把它作为跨 shard 的稳定消息身份，避免手机回灌造成重复；
//   3. server_id 缺失时使用 source table + local_id + create_time 作为保守兜底；
//   4. sync 只 upsert，不删除 archive-only 历史，因此微信后来删除消息不会抹掉归档；
//   5. 文件尚未落盘不是“消息导入失败”，而是显式 media_items.remote_pending。

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/r266-tech/wechat-cli/internal/config"
	"github.com/r266-tech/wechat-cli/internal/wcdb"
	"github.com/r266-tech/wechat-cli/internal/wxkind"
	"github.com/r266-tech/wechat-cli/internal/wxparse"
)

const archiveSchemaVersion = 1
const archiveMinLocalID int64 = -1 << 63

type archiveTalkerMeta struct {
	Username string
	Display  string
}

type archiveSyncStats struct {
	SourceShards      int
	SourceTables      int
	SourceRows        int64
	Upserts           int64
	UnresolvedTables  int
	MediaLocalReady   int64
	MediaRemotePending int64
	StartedAt         time.Time
	FinishedAt        time.Time
}

type archiveMediaItem struct {
	ItemKey    string
	Family     string
	Status     string
	Name       string
	Size       int64
	MD5        string
	LocalPath  string
	CDNURL     string
	CDNKey     string
	SourceKind string
}

// runArchiveCLI 保持 archive 子命令和主 CLI 一样使用稳定 JSON envelope。
// init/sync 会写 wechat-cli 自己的归档目录，所以 strict-read-only 模式明确拒绝；
// status/verify/search 只读 archive/source，可以继续使用。
func runArchiveCLI(args []string, opts cliOptions) {
	if len(args) == 0 {
		runCLIHelp("archive", opts)
		return
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	flags := parseKVFlags(args[1:])
	if sub == "search" {
		if keyword := firstPositional(args[1:]); keyword != "" && flags["keyword"] == nil {
			flags["keyword"] = keyword
		}
	}
	if (sub == "init" || sub == "sync") && strictReadOnlyMode() {
		exitCLIError(opts, 1, "strict_read_only", "archive init/sync writes the canonical archive and is disabled in strict read-only mode", "archive", "archive "+sub)
		return
	}

	srv := &server{}
	var (
		data any
		err  error
	)
	switch sub {
	case "init":
		data, err = srv.archiveInit(flags)
	case "sync":
		data, err = srv.archiveSync(flags)
	case "status":
		data, err = srv.archiveStatus(flags)
	case "verify":
		data, err = srv.archiveVerify(flags)
	case "search":
		data, err = srv.archiveSearch(flags)
	default:
		exitCLIError(opts, 2, "invalid_archive_command", fmt.Sprintf("unknown archive subcommand %q", sub), "archive", "archive")
		return
	}
	if err != nil {
		exitCLIError(opts, 1, "archive_error", err.Error(), "archive", "archive "+sub)
		return
	}
	writeCLISuccess("archive", "archive "+sub, data, opts)
}

func (s *server) archiveInit(a map[string]any) (any, error) {
	root, err := s.archiveRoot(a, true)
	if err != nil {
		return nil, err
	}
	db, err := openArchiveDB(root, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureArchiveSchema(db); err != nil {
		return nil, err
	}
	if err := s.writeArchiveIdentityMeta(db); err != nil {
		return nil, err
	}
	return archiveStatusData(db, root)
}

func (s *server) archiveSync(a map[string]any) (any, error) {
	if err := s.ensure(); err != nil {
		return nil, err
	}
	root, err := s.archiveRoot(a, true)
	if err != nil {
		return nil, err
	}
	db, err := openArchiveDB(root, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureArchiveSchema(db); err != nil {
		return nil, err
	}
	if err := s.writeArchiveIdentityMeta(db); err != nil {
		return nil, err
	}

	batchSize := getInt(a, "batch_size", 1000)
	if batchSize < 100 {
		batchSize = 100
	}
	if batchSize > 5000 {
		batchSize = 5000
	}

	stats := archiveSyncStats{StartedAt: time.Now()}
	talkerMap := s.archiveBuildTalkerMap()
	shards, err := s.archiveMessageShardNames()
	if err != nil {
		return nil, err
	}
	stats.SourceShards = len(shards)

	upsertMessage, err := db.Prepare(`INSERT INTO messages(
		archive_id, source_shard, source_table, talker, talker_display,
		local_id, server_id_str, create_time, sort_seq, sender_wxid, sender,
		is_from_me, raw_type, base_kind, subtype, kind, text, canonical_json,
		raw_content, packed_info, source_content, parse_error, parser_version, updated_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(archive_id) DO UPDATE SET
		source_shard=excluded.source_shard,
		source_table=excluded.source_table,
		talker=excluded.talker,
		talker_display=excluded.talker_display,
		local_id=excluded.local_id,
		server_id_str=excluded.server_id_str,
		create_time=excluded.create_time,
		sort_seq=excluded.sort_seq,
		sender_wxid=excluded.sender_wxid,
		sender=excluded.sender,
		is_from_me=excluded.is_from_me,
		raw_type=excluded.raw_type,
		base_kind=excluded.base_kind,
		subtype=excluded.subtype,
		kind=excluded.kind,
		text=excluded.text,
		canonical_json=excluded.canonical_json,
		raw_content=excluded.raw_content,
		packed_info=excluded.packed_info,
		source_content=excluded.source_content,
		parse_error=excluded.parse_error,
		parser_version=excluded.parser_version,
		updated_at=excluded.updated_at`)
	if err != nil {
		return nil, err
	}
	defer upsertMessage.Close()

	deleteMedia, err := db.Prepare(`DELETE FROM media_items WHERE message_archive_id=?`)
	if err != nil {
		return nil, err
	}
	defer deleteMedia.Close()
	insertMedia, err := db.Prepare(`INSERT INTO media_items(
		message_archive_id, item_key, family, status, name, size, md5,
		local_path, cdn_url, cdn_key, source_kind, updated_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(message_archive_id, item_key) DO UPDATE SET
		family=excluded.family, status=excluded.status, name=excluded.name,
		size=excluded.size, md5=excluded.md5, local_path=excluded.local_path,
		cdn_url=excluded.cdn_url, cdn_key=excluded.cdn_key,
		source_kind=excluded.source_kind, updated_at=excluded.updated_at`)
	if err != nil {
		return nil, err
	}
	defer insertMedia.Close()

	for _, shardName := range shards {
		sourceDB, err := s.openDB("message", shardName)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", shardName, err)
		}
		if err := db.Exec("BEGIN IMMEDIATE"); err != nil {
			sourceDB.Close()
			return nil, fmt.Errorf("archive begin %s: %w", shardName, err)
		}
		committed := false
		func() {
			defer sourceDB.Close()
			tables, tableErr := archiveMessageTables(sourceDB)
			if tableErr != nil {
				err = fmt.Errorf("list %s message tables: %w", shardName, tableErr)
				return
			}
			n2i, _ := loadName2Id(sourceDB)
			for _, tableName := range tables {
				stats.SourceTables++
				meta, known := talkerMap[strings.ToLower(tableName)]
				if !known {
					meta.Username = "hash:" + strings.TrimPrefix(strings.TrimPrefix(tableName, "Msg_"), "msg_")
					stats.UnresolvedTables++
				}
				if tableErr = s.archiveSyncTable(sourceDB, db, upsertMessage, deleteMedia, insertMedia, shardName, tableName, meta, n2i, batchSize, &stats); tableErr != nil {
					err = tableErr
					return
				}
			}
			if tableErr = db.Exec("COMMIT"); tableErr != nil {
				err = fmt.Errorf("archive commit %s: %w", shardName, tableErr)
				return
			}
			committed = true
		}()
		if !committed {
			_ = db.Exec("ROLLBACK")
		}
		if err != nil {
			return nil, err
		}
	}

	stats.FinishedAt = time.Now()
	if err := db.ExecArgs(`INSERT INTO sync_runs(
		started_at, finished_at, source_shards, source_tables, source_rows,
		upserts, unresolved_tables, media_local_ready, media_remote_pending, ok
	) VALUES(?,?,?,?,?,?,?,?,?,1)`,
		stats.StartedAt.Unix(), stats.FinishedAt.Unix(), stats.SourceShards,
		stats.SourceTables, stats.SourceRows, stats.Upserts, stats.UnresolvedTables,
		stats.MediaLocalReady, stats.MediaRemotePending); err != nil {
		return nil, err
	}
	if err := setArchiveMeta(db, "last_sync_at", strconv.FormatInt(stats.FinishedAt.Unix(), 10)); err != nil {
		return nil, err
	}

	status, err := archiveStatusData(db, root)
	if err != nil {
		return nil, err
	}
	status["sync"] = map[string]any{
		"source_shards":       stats.SourceShards,
		"source_tables":       stats.SourceTables,
		"source_rows":         stats.SourceRows,
		"upserts":             stats.Upserts,
		"unresolved_tables":   stats.UnresolvedTables,
		"media_local_ready":   stats.MediaLocalReady,
		"media_remote_pending": stats.MediaRemotePending,
		"elapsed_ms":          stats.FinishedAt.Sub(stats.StartedAt).Milliseconds(),
	}
	return status, nil
}

func (s *server) archiveSyncTable(sourceDB, archiveDB *wcdb.DB, upsertMessage, deleteMedia, insertMedia *wcdb.Stmt, shardName, tableName string, meta archiveTalkerMeta, n2i map[int64]string, batchSize int, stats *archiveSyncStats) error {
	selectSQL, err := archiveMessageSelectSQL(sourceDB, tableName)
	if err != nil {
		return fmt.Errorf("%s/%s schema: %w", shardName, tableName, err)
	}
	lastLocalID := archiveMinLocalID
	selfWxid := s.selfWxid()
	for {
		rows, err := sourceDB.Query(selectSQL, lastLocalID, batchSize)
		if err != nil {
			return fmt.Errorf("%s/%s read after local_id=%d: %w", shardName, tableName, lastLocalID, err)
		}
		if len(rows) == 0 {
			break
		}
		if n2i != nil {
			rows = resolveSenders(rows, n2i)
		}
		rows = enrichMessages(decodeFields(rows, "message_content", "source"))
		for _, row := range rows {
			row["talker"] = meta.Username
			row["talker_display_name"] = meta.Display
			baseKind, subtype, kindName := wxkind.Unpack(rowInt64(row, "local_type"))
			row["base_kind"] = baseKind
			row["subtype"] = subtype
			row["kind_name"] = kindName
			if sid := rowInt64(row, "server_id"); sid != 0 {
				row["server_id_str"] = strconv.FormatInt(sid, 10)
			}
			if selfWxid != "" {
				row["is_from_me"] = rowString(row, "sender_wxid") == selfWxid
			}
		}
		s.attachMessageDisplayNames(rows)

		for _, row := range rows {
			localID := rowInt64(row, "local_id")
			if localID <= lastLocalID {
				return fmt.Errorf("%s/%s local_id did not advance: previous=%d current=%d", shardName, tableName, lastLocalID, localID)
			}
			lastLocalID = localID
			archiveID := archiveMessageIdentity(tableName, row)
			canonical := agentMessage(row)
			canonicalJSON, marshalErr := json.Marshal(canonical)
			if marshalErr != nil {
				return fmt.Errorf("marshal canonical %s: %w", archiveID, marshalErr)
			}
			parsed := mapAny(row["message_content_parsed"])
			parseError := stringMapValue(parsed, "parse_error")
			packed, _ := row["packed_info_data"].([]byte)
			isFromMe := false
			if v, ok := row["is_from_me"].(bool); ok {
				isFromMe = v
			}
			now := time.Now().Unix()
			if err := upsertMessage.Exec(
				archiveID, shardName, tableName, meta.Username, meta.Display,
				localID, rowString(row, "server_id_str"), rowInt64(row, "create_time"), rowInt64(row, "sort_seq"),
				rowString(row, "sender_wxid"), agentMessageSender(row), isFromMe,
				rowInt64(row, "local_type"), rowInt64(row, "base_kind"), rowInt64(row, "subtype"), rowString(row, "kind_name"),
				stringMapValue(canonical, "text"), string(canonicalJSON), rowString(row, "message_content"), packed, rowString(row, "source"),
				parseError, appVersion, now,
			); err != nil {
				return fmt.Errorf("upsert %s: %w", archiveID, err)
			}
			stats.SourceRows++
			stats.Upserts++

			media := s.archiveMediaItems(meta.Username, row)
			if len(media) > 0 {
				if err := deleteMedia.Exec(archiveID); err != nil {
					return fmt.Errorf("clear media %s: %w", archiveID, err)
				}
				for _, item := range media {
					if item.Status == "local_ready" {
						stats.MediaLocalReady++
					} else if item.Status == "remote_pending" {
						stats.MediaRemotePending++
					}
					if err := insertMedia.Exec(
						archiveID, item.ItemKey, item.Family, item.Status, item.Name, item.Size, item.MD5,
						item.LocalPath, item.CDNURL, item.CDNKey, item.SourceKind, now,
					); err != nil {
						return fmt.Errorf("insert media %s/%s: %w", archiveID, item.ItemKey, err)
					}
				}
			}
		}
		if len(rows) < batchSize {
			break
		}
	}
	return nil
}

func (s *server) archiveStatus(a map[string]any) (any, error) {
	root, err := s.archiveRoot(a, false)
	if err != nil {
		return nil, err
	}
	db, err := openArchiveDB(root, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return archiveStatusData(db, root)
}

func (s *server) archiveVerify(a map[string]any) (any, error) {
	if err := s.ensure(); err != nil {
		return nil, err
	}
	root, err := s.archiveRoot(a, false)
	if err != nil {
		return nil, err
	}
	db, err := openArchiveDB(root, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	sourceIDs, sourceRows, sourceTables, err := s.archiveSourceIdentities()
	if err != nil {
		return nil, err
	}
	archiveRows, err := db.Query(`SELECT archive_id FROM messages`)
	if err != nil {
		return nil, err
	}
	archiveIDs := make(map[string]struct{}, len(archiveRows))
	for _, row := range archiveRows {
		archiveIDs[rowString(row, "archive_id")] = struct{}{}
	}
	missing := make([]string, 0, 20)
	missingCount := 0
	for id := range sourceIDs {
		if _, ok := archiveIDs[id]; ok {
			continue
		}
		missingCount++
		if len(missing) < 20 {
			missing = append(missing, id)
		}
	}
	archiveOnly := 0
	for id := range archiveIDs {
		if _, ok := sourceIDs[id]; !ok {
			archiveOnly++
		}
	}
	ftsCount, err := archiveScalarInt(db, `SELECT COUNT(*) AS n FROM messages_fts`)
	if err != nil {
		return nil, err
	}
	parseErrors, err := archiveScalarInt(db, `SELECT COUNT(*) AS n FROM messages WHERE parse_error <> ''`)
	if err != nil {
		return nil, err
	}
	duplicateSourceRows := sourceRows - int64(len(sourceIDs))
	ok := missingCount == 0 && ftsCount == int64(len(archiveIDs))
	return map[string]any{
		"ok":                    ok,
		"archive_root":          root,
		"source_tables":         sourceTables,
		"source_raw_rows":       sourceRows,
		"source_unique_messages": len(sourceIDs),
		"source_duplicate_rows": duplicateSourceRows,
		"archive_messages":      len(archiveIDs),
		"source_missing_in_archive": missingCount,
		"missing_sample":        missing,
		"archive_only_messages": archiveOnly,
		"fts_rows":              ftsCount,
		"parse_error_rows":      parseErrors,
		"note":                  "archive-only messages are preserved history and are not a verification failure",
	}, nil
}

func (s *server) archiveSearch(a map[string]any) (any, error) {
	root, err := s.archiveRoot(a, false)
	if err != nil {
		return nil, err
	}
	keyword := strings.TrimSpace(getStr(a, "keyword"))
	if keyword == "" {
		return nil, fmt.Errorf("keyword is required")
	}
	limit := getInt(a, "limit", 20)
	if limit < 1 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	db, err := openArchiveDB(root, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	// 普通用户输入按“完整短语”查询，避免 FTS5 把标点解释为运算符。
	ftsQuery := `"` + strings.ReplaceAll(keyword, `"`, `""`) + `"`
	rows, err := db.Query(`SELECT m.archive_id, m.talker, m.talker_display,
		m.create_time, m.sender, m.sender_wxid, m.kind, m.text, m.canonical_json
		FROM messages_fts f JOIN messages m ON m.id=f.rowid
		WHERE messages_fts MATCH ?
		ORDER BY m.create_time DESC LIMIT ?`, ftsQuery, limit)
	if err != nil {
		return nil, err
	}
	results := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		item := map[string]any{
			"archive_id":     rowString(row, "archive_id"),
			"talker":         rowString(row, "talker"),
			"talker_display": rowString(row, "talker_display"),
			"create_time":    rowInt64(row, "create_time"),
			"time_iso":       cliFormatUnixISO(rowInt64(row, "create_time")),
			"sender":         rowString(row, "sender"),
			"sender_wxid":    rowString(row, "sender_wxid"),
			"kind":           rowString(row, "kind"),
			"text":           rowString(row, "text"),
		}
		if raw := rowString(row, "canonical_json"); raw != "" {
			var canonical map[string]any
			if json.Unmarshal([]byte(raw), &canonical) == nil {
				item["message"] = canonical
			}
		}
		results = append(results, item)
	}
	return map[string]any{
		"archive_root": root,
		"keyword":      keyword,
		"returned":     len(results),
		"messages":     results,
	}, nil
}

// archiveRoot 在 explicit root 存在时完全不依赖 live WeChat，因此未来即使
// 微信升级到无法解密，已有 archive 仍能 status/search。只有使用默认路径时
// 才读取当前 config 中的 wxid 来做账号隔离。
func (s *server) archiveRoot(a map[string]any, create bool) (string, error) {
	root := strings.TrimSpace(getStr(a, "root"))
	if root == "" {
		root = strings.TrimSpace(os.Getenv("WECHAT_CLI_ARCHIVE_ROOT"))
	}
	if root == "" {
		cfg, err := config.Load()
		if err != nil {
			return "", err
		}
		wxid := strings.TrimSpace(cfg.Wxid)
		if wxid == "" {
			wxid = "default"
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".wechat-cli", "archive", wxid)
	}
	root = filepath.Clean(root)
	if create {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", err
		}
	}
	return root, nil
}

func openArchiveDB(root string, writable bool) (*wcdb.DB, error) {
	lib, err := findWCDB()
	if err != nil {
		return nil, err
	}
	if err := wcdb.Bootstrap(lib); err != nil {
		return nil, err
	}
	path := filepath.Join(root, "archive.sqlite")
	if !writable {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("archive does not exist at %s: %w", path, err)
		}
	}
	return wcdb.OpenPlain(path, writable)
}

func ensureArchiveSchema(db *wcdb.DB) error {
	schema := `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
CREATE TABLE IF NOT EXISTS archive_meta(
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS messages(
  id INTEGER PRIMARY KEY,
  archive_id TEXT NOT NULL UNIQUE,
  source_shard TEXT NOT NULL,
  source_table TEXT NOT NULL,
  talker TEXT NOT NULL,
  talker_display TEXT NOT NULL DEFAULT '',
  local_id INTEGER NOT NULL,
  server_id_str TEXT NOT NULL DEFAULT '',
  create_time INTEGER NOT NULL DEFAULT 0,
  sort_seq INTEGER NOT NULL DEFAULT 0,
  sender_wxid TEXT NOT NULL DEFAULT '',
  sender TEXT NOT NULL DEFAULT '',
  is_from_me INTEGER NOT NULL DEFAULT 0,
  raw_type INTEGER NOT NULL DEFAULT 0,
  base_kind INTEGER NOT NULL DEFAULT 0,
  subtype INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL DEFAULT '',
  text TEXT NOT NULL DEFAULT '',
  canonical_json TEXT NOT NULL DEFAULT '{}',
  raw_content TEXT NOT NULL DEFAULT '',
  packed_info BLOB,
  source_content TEXT NOT NULL DEFAULT '',
  parse_error TEXT NOT NULL DEFAULT '',
  parser_version TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_messages_talker_time ON messages(talker, create_time, id);
CREATE INDEX IF NOT EXISTS idx_messages_time ON messages(create_time, id);
CREATE INDEX IF NOT EXISTS idx_messages_server ON messages(server_id_str);
CREATE TABLE IF NOT EXISTS media_items(
  id INTEGER PRIMARY KEY,
  message_archive_id TEXT NOT NULL,
  item_key TEXT NOT NULL,
  family TEXT NOT NULL,
  status TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  md5 TEXT NOT NULL DEFAULT '',
  local_path TEXT NOT NULL DEFAULT '',
  cdn_url TEXT NOT NULL DEFAULT '',
  cdn_key TEXT NOT NULL DEFAULT '',
  source_kind TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0,
  UNIQUE(message_archive_id, item_key)
);
CREATE INDEX IF NOT EXISTS idx_media_status ON media_items(status, family);
CREATE TABLE IF NOT EXISTS sync_runs(
  id INTEGER PRIMARY KEY,
  started_at INTEGER NOT NULL,
  finished_at INTEGER NOT NULL,
  source_shards INTEGER NOT NULL,
  source_tables INTEGER NOT NULL,
  source_rows INTEGER NOT NULL,
  upserts INTEGER NOT NULL,
  unresolved_tables INTEGER NOT NULL,
  media_local_ready INTEGER NOT NULL,
  media_remote_pending INTEGER NOT NULL,
  ok INTEGER NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
  text, talker, sender,
  content='messages', content_rowid='id',
  tokenize='unicode61'
);
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, text, talker, sender)
  VALUES (new.id, new.text, new.talker, new.sender);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, text, talker, sender)
  VALUES('delete', old.id, old.text, old.talker, old.sender);
END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, text, talker, sender)
  VALUES('delete', old.id, old.text, old.talker, old.sender);
  INSERT INTO messages_fts(rowid, text, talker, sender)
  VALUES (new.id, new.text, new.talker, new.sender);
END;`
	if err := db.Exec(schema); err != nil {
		return fmt.Errorf("create archive schema: %w", err)
	}
	if err := setArchiveMeta(db, "schema_version", strconv.Itoa(archiveSchemaVersion)); err != nil {
		return err
	}
	return nil
}

func (s *server) writeArchiveIdentityMeta(db *wcdb.DB) error {
	if err := s.ensure(); err != nil {
		return err
	}
	for key, value := range map[string]string{
		"wxid":           s.cfg.Wxid,
		"source_db_root": s.cfg.DBRoot,
		"parser_version": appVersion,
	} {
		if err := setArchiveMeta(db, key, value); err != nil {
			return err
		}
	}
	return nil
}

func setArchiveMeta(db *wcdb.DB, key, value string) error {
	return db.ExecArgs(`INSERT INTO archive_meta(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
}

func archiveStatusData(db *wcdb.DB, root string) (map[string]any, error) {
	messages, err := archiveScalarInt(db, `SELECT COUNT(*) AS n FROM messages`)
	if err != nil {
		return nil, err
	}
	localReady, err := archiveScalarInt(db, `SELECT COUNT(*) AS n FROM media_items WHERE status='local_ready'`)
	if err != nil {
		return nil, err
	}
	remotePending, err := archiveScalarInt(db, `SELECT COUNT(*) AS n FROM media_items WHERE status='remote_pending'`)
	if err != nil {
		return nil, err
	}
	ftsRows, err := archiveScalarInt(db, `SELECT COUNT(*) AS n FROM messages_fts`)
	if err != nil {
		return nil, err
	}
	metaRows, err := db.Query(`SELECT key,value FROM archive_meta ORDER BY key`)
	if err != nil {
		return nil, err
	}
	meta := map[string]any{}
	for _, row := range metaRows {
		meta[rowString(row, "key")] = rowString(row, "value")
	}
	path := filepath.Join(root, "archive.sqlite")
	size := int64(0)
	if st, statErr := os.Stat(path); statErr == nil {
		size = st.Size()
	}
	return map[string]any{
		"archive_root": root,
		"database":     path,
		"database_size_bytes": size,
		"messages":     messages,
		"fts_rows":     ftsRows,
		"media": map[string]any{
			"local_ready":   localReady,
			"remote_pending": remotePending,
		},
		"meta": meta,
	}, nil
}

func archiveScalarInt(db *wcdb.DB, sql string, args ...any) (int64, error) {
	rows, err := db.Query(sql, args...)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rowInt64(rows[0], "n"), nil
}

func (s *server) archiveMessageShardNames() ([]string, error) {
	if err := s.ensure(); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.cfg.DBRoot, "db_storage", "message")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && msgShardRE.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("no message shards found under %s", dir)
	}
	return names, nil
}

func archiveMessageTables(db *wcdb.DB) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Msg_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	tables := make([]string, 0, len(rows))
	for _, row := range rows {
		if name := rowString(row, "name"); name != "" {
			tables = append(tables, name)
		}
	}
	return tables, nil
}

func archiveMessageSelectSQL(db *wcdb.DB, tableName string) (string, error) {
	rows, err := db.Query(`PRAGMA table_info(` + quoteIdent(tableName) + `)`)
	if err != nil {
		return "", err
	}
	cols := map[string]bool{}
	for _, row := range rows {
		cols[strings.ToLower(rowString(row, "name"))] = true
	}
	for _, required := range []string{"local_id", "server_id", "local_type", "create_time", "message_content"} {
		if !cols[required] {
			return "", fmt.Errorf("required column %s missing", required)
		}
	}
	expr := func(name string, fallback string) string {
		if cols[name] {
			return name
		}
		return fallback + " AS " + name
	}
	return fmt.Sprintf(`SELECT local_id, server_id, local_type,
		%s, %s, create_time, %s, message_content, %s, %s
		FROM %s WHERE local_id > ? ORDER BY local_id ASC LIMIT ?`,
		expr("sort_seq", "0"), expr("real_sender_id", "0"), expr("status", "0"),
		expr("packed_info_data", "NULL"), expr("source", "NULL"), quoteIdent(tableName)), nil
}

// archiveBuildTalkerMap 只是为 Msg_<md5> 反推出可读 username/display name。
// 即使某个旧会话已经不在 contact/session，sync 也不会跳过它，而是以 hash:...
// 占位归档；这正是“完整性优先于漂亮名称”的设计。
func (s *server) archiveBuildTalkerMap() map[string]archiveTalkerMeta {
	out := map[string]archiveTalkerMeta{}
	add := func(username, display string) {
		username = strings.TrimSpace(username)
		if username == "" {
			return
		}
		key := strings.ToLower("Msg_" + talkerHash(username))
		current := out[key]
		if current.Username == "" || (current.Display == "" && display != "") {
			out[key] = archiveTalkerMeta{Username: username, Display: strings.TrimSpace(display)}
		}
	}
	if raw, err := s.toolContacts(map[string]any{"limit": int64(20000)}); err == nil {
		if rows, ok := raw.([]wcdb.Row); ok {
			for _, row := range rows {
				add(rowString(row, "username"), rowString(row, "display_name"))
			}
		}
	}
	if raw, err := s.toolSessions(map[string]any{"limit": int64(5000)}); err == nil {
		if rows, ok := cliResultRows(raw); ok {
			for _, row := range rows {
				add(rowString(row, "username"), rowString(row, "display_name"))
			}
		}
	}
	return out
}

func archiveMessageIdentity(tableName string, row wcdb.Row) string {
	if sid := rowInt64(row, "server_id"); sid != 0 {
		return "server:" + strconv.FormatInt(sid, 10)
	}
	return fmt.Sprintf("local:%s:%d:%d", strings.ToLower(tableName), rowInt64(row, "local_id"), rowInt64(row, "create_time"))
}

func (s *server) archiveSourceIdentities() (map[string]struct{}, int64, int, error) {
	shards, err := s.archiveMessageShardNames()
	if err != nil {
		return nil, 0, 0, err
	}
	ids := map[string]struct{}{}
	var rawRows int64
	tableCount := 0
	for _, shardName := range shards {
		db, err := s.openDB("message", shardName)
		if err != nil {
			return nil, rawRows, tableCount, err
		}
		tables, err := archiveMessageTables(db)
		if err != nil {
			db.Close()
			return nil, rawRows, tableCount, err
		}
		for _, table := range tables {
			tableCount++
			last := archiveMinLocalID
			for {
				rows, err := db.Query(fmt.Sprintf(`SELECT local_id, server_id, create_time FROM %s WHERE local_id > ? ORDER BY local_id ASC LIMIT 5000`, quoteIdent(table)), last)
				if err != nil {
					db.Close()
					return nil, rawRows, tableCount, err
				}
				if len(rows) == 0 {
					break
				}
				for _, row := range rows {
					last = rowInt64(row, "local_id")
					ids[archiveMessageIdentity(table, row)] = struct{}{}
					rawRows++
				}
				if len(rows) < 5000 {
					break
				}
			}
		}
		db.Close()
	}
	return ids, rawRows, tableCount, nil
}

func (s *server) archiveMediaItems(talker string, row wcdb.Row) []archiveMediaItem {
	kind := rowString(row, "kind_name")
	raw := rowString(row, "message_content")
	createTime := rowInt64(row, "create_time")
	var out []archiveMediaItem

	if kind == "file" && raw != "" {
		var msg xmlMsgAppmsg
		if xml.Unmarshal([]byte(wxparse.StripMsgPrefix(raw)), &msg) == nil {
			name := strings.TrimSpace(msg.AppMsg.Title)
			paths := s.localMediaPaths(talker, createTime, "file", "", []string{name})
			item := archiveMediaItem{
				ItemKey:    "direct:file",
				Family:     "file",
				Status:     "remote_pending",
				Name:       name,
				Size:       msg.AppMsg.AppAttach.TotalLen,
				CDNURL:     strings.TrimSpace(msg.AppMsg.AppAttach.CDNAttachURL),
				CDNKey:     strings.TrimSpace(msg.AppMsg.AppAttach.AesKey),
				SourceKind: "direct_file",
			}
			if len(paths) > 0 {
				item.Status = "local_ready"
				item.LocalPath = paths[0]
			}
			out = append(out, item)
		}
	}

	if kind == "forward_chat" && raw != "" {
		if items, err := wxparse.ForwardItems(raw, 5); err == nil {
			out = append(out, s.archiveForwardFileItems(talker, createTime, items, "forward")...)
		}
	}
	return out
}

func (s *server) archiveForwardFileItems(talker string, fallbackTime int64, items []wxparse.ForwardItem, prefix string) []archiveMediaItem {
	var out []archiveMediaItem
	for i, item := range items {
		itemKey := fmt.Sprintf("%s:%d", prefix, i)
		if item.DataID != "" {
			itemKey += ":" + item.DataID
		}
		if item.DataType == 8 {
			createTime := item.SrcMsgCreateTime
			if createTime == 0 {
				createTime = fallbackTime
			}
			paths := s.localMediaPaths(talker, createTime, "file", "", []string{item.DataTitle})
			media := archiveMediaItem{
				ItemKey:    itemKey,
				Family:     "file",
				Status:     "remote_pending",
				Name:       strings.TrimSpace(item.DataTitle),
				Size:       item.DataSize,
				MD5:        strings.ToLower(strings.TrimSpace(item.FullMD5)),
				CDNURL:     strings.TrimSpace(item.CDNDataURL),
				CDNKey:     strings.TrimSpace(item.CDNDataKey),
				SourceKind: "forward_file",
			}
			if len(paths) > 0 {
				media.Status = "local_ready"
				media.LocalPath = paths[0]
			}
			out = append(out, media)
		}
		if len(item.NestedItems) > 0 {
			out = append(out, s.archiveForwardFileItems(talker, fallbackTime, item.NestedItems, itemKey+":nested")...)
		}
	}
	return out
}
