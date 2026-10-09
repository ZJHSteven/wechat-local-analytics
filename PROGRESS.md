# 项目状态快照

## 当前结论（必须最新）
- 2026-10-09：已在 `fix/forward-rec-media` 完成统一的合并转发本地媒体回填。父 forward 消息的 `packed_info_data` 可提取稳定 record-id；GUI 已下载/点开的媒体可从 `msg/attach/<chat-hash>/<parent-month>/Rec/<record-id>/Img|V|F` 确定性发现。无扩展名 `Rec/Img/<index>` 仍是标准 WeChat V4 图片容器，现直接复用已有 image_key/image_xor_key 解码器。
- 2026-10-09：图片 source-message 缩略图/普通 attach/temp 可读副本/Rec 大图与已有 `_h` 原图现在统一进入候选池，由现有图片质量评分按缩略图标记、像素面积、文件大小和 variant 排名选择 Agent 可读的最佳版本；不会因为“已有 157×210 可读缩略图”就提前停止。
- 2026-10-09：`history/timeline` 与专用 `media` 入口已复用同一套 forward parser/resolver；`media --local-id <forward>` 会展开 `forward_chat.items[].images/videos/files`。forward file 也会消费本地 media hint，命中 `Rec/F` 后返回 `readable:true`，并同步供 canonical archive 的 forward-file manifest 判断 `local_ready`。
- 2026-10-09 真机回归：文件传输助手 10-07 21:48 的上午实验转发自动选出 1280×1707 等 Rec 大图，并保留两张已有 4032×3024 `_h`；“病原小组” 10-04 20:51 / local_id=51 的 15 张 Transwell 图全部自动返回 960×1280 Rec 大图，另两张已有 `_h` 继续使用 4032×3024；数学建模群 09-12 00:29 / local_id=270 的 7 个 ZIP/XLSX 全部从 `Rec/F/0..6` 恢复为 `readable:true`，包括 `result4_rechecked (1).xlsx` 精确命中 `F/6`。此前记录的 `forward_file_not_resolved` 缺口已被该统一 resolver 实际解决。
- 现状：`wechat-cli` 1.6.20 已在 Windows 微信 4.1.9.57 + Sandboxie 环境完成 DB key、会话解析、timeline、search、JSONL/Markdown 导出的真实数据验证。
- 已完成：找到 r266 幸存源码；Windows 编译成功；构建 SQLCipher 兼容 `libWCDB.dll`；首次 `cache refresh --force` 成功获取 24 个 DB key；仝立炎已知窗口返回精确 73 条。
- 正在做：基于用户刚从手机回灌后的更完整历史，重新建立全库回归基线；调查微信原生语音转写缓存。回灌后 cache 已从 24 个 DB key / 855 会话增长到 26 个 DB key / 1238 会话。
- 图片 key 根因已定位：幸存源码的 Windows DB key 已内置扫描，但 `RunImageKey` 仍硬依赖原 release 的 `wxkey.exe`；DMCA 后该 helper 缺失，导致每次图片读取都只留下 `image_decode_needs_image_key`。现已加入 Windows 进程内图片 key 扫描实现，待真实图片回归验证。
- 第一版内置图片扫描在多 Weixin 子进程场景耗时偏长；已改为优先扫描 DB key 实际命中的 `KeyPID`，并把 ASCII 候选识别改成线性 run 扫描，避免逐字节 32 次重复检查。
- 已确认微信原生“语音转文字”不是只能存在 GUI 状态里：完成结果保存在消息表 `packed_info_data` 的 protobuf-like 字段中。现已把该字段加入 live message 查询，并优先按 `field 5 -> status(field 1)=2 -> text(field 2)` 提取为 `engine=wechat-native`；外部 ASR 只保留为没有原生缓存时的可选 fallback。
- 新增开发回归探针 `cmd/wechat-cli-probe-native-transcripts`：只读扫描所有编号 message shard，直接复用 WCDB reader + `NativeVoiceTranscript`，用于在真实微信数据库中验证原生语音转写缓存链路。
- 真库探针已命中多条微信原生转写；进一步用正式 `context` 交叉验证时发现 context 专用 SELECT 漏取 `packed_info_data`，现已补齐，避免 timeline 与 context/search-context 对同一语音产生不一致输出。
- 晋升条件已满足：真实 `context` 已对命中样本返回 `engine=wechat-native / status=ok`，正文直接进入 Agent 消息文本；图片、朋友圈、收藏、跨 shard、搜索/上下文/导出此前也均已通过当前机器真数据验证。
- 下一步：运行 CLI 回归测试、全库类型扫描、重点媒体/复杂消息抽样；只修影响 AI 完整理解的缺口。
- 第二阶段设计已明确：现有 `tail/watch --follow` 实现是每轮 `read_events` 后 `Sleep(poll_interval)`，默认 2 秒轮询；长期 notifier 将改为“文件系统事件唤醒 + cursor 权威补读 + 低频 reconciliation”，不把轮询作为主要实时路径。
- 通知聚合采用 per-chat trailing debounce + max-wait，而不是每条消息启动 Agent；原始消息先持久化，再由 PI 对 burst 做 `ignore/notify/task/awaiting_reply` 分类。
- 当前账号磁盘逻辑大小实测约 16.29 GiB：`msg` 约 14.42 GiB，`db_storage` 约 1.56 GiB；真正大头是文件/图片/视频附件，不是消息 SQLite。
- `message_0..6.db` 实测 `freelist_count=0`；`message_0.db` 为 `secure_delete=1`、`auto_vacuum=0`，当前没有可见的大量 SQLite freelist 空洞。
- WeChat 4.x 的 `hardlink.db` 确实在做媒体/文件去重；已抽样验证同一 10.6MB PPT 的 4 个不同文件名实际是同一 NTFS 文件的 4 个 hardlink，而不是四份物理副本。
- 第二阶段 canonical archive 已真实落地在 Sandbox 外：`D:\WeChatArchive\wxid_4mty0mzusr4k12\archive.sqlite`。首次全量扫描 8 个 message/biz shard、1609 个 `Msg_*` 表，共读取 978406 条源行，按稳定消息身份去重后得到 978396 条 canonical 消息；FTS 行数与 canonical 消息严格一致。
- Archive schema 已升级到 v2，`archive sync` 改为 per-table watermark 幂等增量同步；一次真实增量验收仅处理 612 条新增消息，核心导入耗时约 2.42 秒。命令总耗时目前仍受 1609 张表枚举/打开的固定成本影响，后续可继续用 dirty-shard / MAX(local_id) 预检优化。
- `archive verify` 已改为按上次同步 watermark 做活数据库一致性验证：最新真实验收为 archive 979030 条、FTS 979030 条、watermark 范围内 `source_missing_in_archive=0`、`archive_only_messages=0`；verify 扫描期间新到的 2 条消息只计入 `new_since_sync=2`，不误报归档损坏。
- Archive 当前只保存消息事实、raw/canonical JSON、FTS 与媒体清单/状态；尚未把全部媒体实体复制到 Archive。现已识别 2677 个 `local_ready` 文件项和 4813 个 `remote_pending` 文件项，后者是后续自动抢救下载的主要队列。
- 文件 parser 已补保留普通 AppMsg 的 `cdnattachurl`；合并转发中的嵌套 file item 也会递归进入媒体清单，便于后续优先尝试 CDN 元数据下载，失败再走微信 UI 下载 fallback。
- 2026-09-12 已用真实合并转发样本确认一个本地文件发现缺口：用户在微信 GUI 手动下载后的文件可落在 `msg/attach/<chat-hash>/<YYYY-MM>/Rec/<record-id>/F/<slot>/<name>`，而当前 `localMediaPaths(..., family="file")` 只检查 `msg/file/<YYYY-MM>/<name>`，因此即使文件已实际落盘，forward item 仍可能返回 `forward_file_not_resolved`。真库同时给出了可用于确定性修复的映射：父 forward 消息的 `message_resource.db` 以 `data_index=0..N-1` 对应转发 item 顺序；`hardlink.db:file_hardlink_info_v4` 记录文件名/大小/MD5、`dir1/dir2` 和 `extra_buffer`，其中 `extra_buffer` 可还原 `Rec/<record-id>/F/<slot>`。修复应优先使用这些数据库键，不做全盘同名文件盲猜。
- 该问题已在自有仓库 `ZJHSteven/wechat-local-analytics` 建立 issue `#1`（`fix(media): 合并转发手动下载文件未从 msg/attach/Rec 回填`），包含真实 Q4 样本、数据库证据、源码缺口、建议实现和验收测试；后续修复工作由 GitHub coding agent 独立处理，主聊天不再阻塞数学建模任务。
- 带 Archive 功能的最新 `wechat-cli.exe` 已重新安装到 `D:\App\wechat-cli`；从 `C:\Users\ZJHSteven\bin\wechat-cli.cmd` 全局入口冷启动 `archive status` 验收成功。

## 本机固定路径
- 正式源码仓库：`D:\Workspace\WeChatCLI`
- 正式运行包：`D:\App\wechat-cli`
- 全局命令入口：`C:\Users\ZJHSteven\bin\wechat-cli.cmd`（该目录已在 PATH）
- 运行方式：默认一次性进程，命令执行完即退出；只有显式使用 `tail/watch --follow` 时才持续运行，不安装常驻守护进程。
- `D:\Workspace\WeChatCliBench` 只保留回归样本与其他对照项目，不再作为正式源码位置。
- 2026-09-03 已完成正式迁移与全局安装；从 `C:\` 冷启动验证 `status`、真实 `wechat-native` 语音、图片解密、朋友圈和收藏均成功。Bench 中旧 `wechat-local-analytics` 源码副本已删除，避免双仓库歧义。

## 关键决策与理由
- 主底座选 `wechat-cli`：CLI/Agent 契约已经完整，避免重复从 `wechatauto-replica` 手写一套命令层。
- `wechatauto-replica` 保留为底层/媒体 fallback：已在当前机器实证 DB、SILK、V2 图片恢复能力。
- `WeChatDataAnalysis` 作为 parser / 多 shard / realtime 的参考实现和对照 oracle，不把大型 GUI/runtime 作为主依赖。
- MemoTrace 不进入运行时依赖：旧 parser 可参考，但当前 4.1.9 媒体 hardlink schema 已出现漂移。
- 第一阶段不做发送和 Web UI：先确保微信版 QQ Chat Exporter 的读取/搜索/导出闭环稳定。

## 常见坑 / 复现方法
- Sandboxie：微信进程看到的是逻辑路径，沙盒外工具必须映射到 `D:\\Sandboxie\\WeChat\\...` 的物理数据根。
- DB key 与图片 key 不是同一套材料；“24/24 DB key 成功”不能推出 V2 `.dat` 图片一定可解。
- 同一联系人消息跨 `message_0.db` / `message_1.db` 等 shard；只取第一个匹配表会漏历史。
- 未知 subtype 必须保留 raw XML/原始字段并输出 warning，不能解释成“没有这条消息”。
- Windows Git Bash/MSYS 在 NTFS 上可能让 `chmod 700` 返回 Permission denied；回归脚本已改为 Windows 只创建输出目录，不把 POSIX mode 当成产品能力测试。
- Windows 图片 key 与 DB key 是独立路径；幸存源码缺外部 `wxkey.exe` 不影响 DB key，却会让 V2 `.dat` 解码失败。Windows 版现在改为直接复用现有只读进程扫描基础设施，不再要求该 helper。
