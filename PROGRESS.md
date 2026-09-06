# 项目状态快照

## 当前结论（必须最新）
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
