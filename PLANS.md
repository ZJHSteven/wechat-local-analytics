# 微信 Chat Exporter 第一阶段 ExecPlan

## 2026-09-12：合并转发手动下载附件自动发现审计

目标：解释并验证“用户已在微信 GUI 中手动下载合并转发附件，但 `wechat-cli` 仍返回 `forward_file_not_resolved`”的根因，并判断是否属于媒体发现/回填设计缺口。

1. 定位 `forward_file_not_resolved`、合并转发 file item、`media_resources` 与本地文件解析代码。
2. 对照当前真机样本：Q4 ZIP / `result4_rechecked.xlsx` 已落在 `msg/attach/.../Rec/...`，但普通文件解析主要命中 `msg/file/...`。
3. 检查 `hardlink.db`、消息 XML / packed 字段是否存在足以把 forward item 映射到 `msg/attach` 实体的稳定键（文件名、大小、md5、attach id、hardlink 记录等）。
4. 判断合理修复层级：优先做确定性本地实体回填；仅在无法唯一匹配时保持 unresolved，禁止按同名文件盲猜。
5. 用真实 Q4 合并转发样本和自动化单测验证；若实现修改，同步更新 `PROGRESS.md`。

## 目标
- 以当前 `wechat-cli` 幸存源码为主底座，做成可长期维护的微信 4.x 本地历史读取/搜索/导出 CLI。
- 当前阶段只解决“稳定读取和导出”，不把实时发送、家庭服务器托管、Web UI 混入主线。
- 任何未知消息类型都必须保留 raw 信息并显式标记，禁止静默丢消息。

## 本轮验收顺序
1. 刷新用户刚从手机回灌后的完整微信数据，重新建立 cache/key 状态。
2. 运行 CLI 自带 Agent/read regression，覆盖 status、sessions、resolve、timeline、search、context、tail、media、export。
3. 扫描全库真实 `local_type/subtype`，建立“实际出现过的消息类型”基线，而不是只依赖单联系人样本。
4. 验证重点消息：文本、图片、语音、视频、文件、链接、引用、合并转发、小程序、拍一拍、位置、名片、系统消息、转账/红包、视频号等。
5. 调查图片 key：确认 DB key 与图片 key 的关系、当前 `wechat-cli` 为何未写入 image key，以及 `wechatauto-replica` 成功路径能否作为 fallback。
6. 调查微信原生语音转文字缓存：优先复用微信已有转写，只有缓存不存在时才考虑外部 ASR fallback。
7. 验收朋友圈、收藏等旁支能力，记录是否可直接供 AI 使用；不因旁支失败阻塞主线。
8. 根据测试结果，只补影响“信息完整/AI 理解”的关键缺口；低价值礼物/电商特殊卡先保证 raw 不丢，不追求漂亮渲染。

## 第二阶段：事件驱动监听与提醒器
- 保留现有 `read_events` / cursor 作为“权威增量读取”层；禁止把文件系统事件本身当成消息事实来源。
- 当前 `tail/watch --follow` 是固定间隔轮询（默认 2 秒、最短 500ms），只作为兼容 fallback，不作为长期通知守护进程的主唤醒机制。
- 主唤醒机制改为监听 `db_storage/message`、`db_storage/session` 下 DB/WAL 的文件变更事件：Windows 用文件系统通知，Linux 未来用 inotify；收到事件只标记 dirty，然后用 cursor 从 DB 补齐真实消息。
- 为防文件通知合并/丢失，允许低频 reconciliation heartbeat（例如 60 秒）校验 cursor；它不是主要实时路径。
- 原始新消息必须先持久化到本地 notifier SQLite，再进入 AI 分类，保证崩溃/重启不丢消息。
- 以会话为单位做 burst/session 聚合，不按单条消息启动 Agent：
  - 私聊 quiet period 初始建议 12~20 秒；群聊初始建议约 30 秒。
  - 每来新消息重置 quiet deadline（trailing-edge debounce）。
  - 同时设置 max-wait 60~90 秒，避免活跃会话一直有消息而永远不触发。
  - 多个已 ready 的 burst 可再做 1~3 秒 micro-batch，一次交给 PI 分类多个会话。
- PI 只做高层语义分类/提取，建议输出 `ignore | notify | task | awaiting_reply`、优先级、摘要、建议动作；不承担消息去重和可靠性。
- 通知/task 实体保存其来源 message IDs，允许后续新 burst 合并、升级优先级或标记 resolved。

## 长期归档 / 灾备
- 建立两个互补层，而不是把每个聊天反复导出成 Markdown：
  1. recovery capsule：当前可解密的 `db_storage` 快照 + 对应 key 配置 + 已验证的 wechat-cli/runtime/source commit，用于未来版本突然失配时恢复旧数据。
  2. canonical archive：规范化 SQLite/FTS 保存消息、联系人、朋友圈、收藏等；媒体使用内容寻址 blob（按 hash 去重），供长期 AI 搜索。
- canonical archive 以 server_id/message identity 做幂等增量导入；未知消息保留 raw 字段和 parser_version。
- 图片优先归档解码后的 JPEG/PNG；语音保存 SILK + 微信原生 transcript（若有）；视频/文件保存原始本地实体。
- 不要求 archive 与微信在线数据目录同盘；未来优先迁到家庭服务器。

### 当前实施批次：Windows canonical archive MVP
1. 在主 CLI 增加 `archive init|sync|status|verify`，默认归档根放在用户可配置目录，不改写微信原始目录。
2. `archive sync` 直接枚举所有 `message_N.db` 的全部 `Msg_*` 表；以 `(talker, server_id)` 为主身份、`(talker, local_id, create_time)` 为兜底身份做幂等 upsert，禁止只遍历最近 session。
3. Archive SQLite 至少保存：稳定消息 ID、talker、sender、时间、kind、展示文本、canonical JSON、raw type/subtype、raw XML/packed 字段、parser/version、源 shard。
4. 同时建立 FTS5 索引，后续让历史搜索优先查询 archive；本批次先验证“全量导入 + 二次 sync 幂等 + verify 数量一致”。
5. 媒体先建立 manifest/pending 队列：已存在本地实体记录 path/size/hash；消息声明有文件但本地不存在时记录 `remote_pending`，不把消息本体判为失败。
6. 合并转发中的嵌套 file item 单独提取到 pending-media：优先保存可见文件名、大小、md5、cdn/aes 元数据；后续下载器优先走可验证的远端下载路径，失败才交给微信 UI 自动化触发下载。
7. 首次全量 archive 验证通过之前禁止清理微信原目录；只有 `archive verify`、抽样读取和 recovery capsule 均通过后才讨论瘦身。

## 后续阶段
- 第三阶段：独立 send adapter（UI Automation/RPA/其它可行路线），读写彻底解耦。
- 第四阶段：家庭服务器托管 Linux 微信/QQ + 监听/归档服务 + 自建 Web UI。

## 通过标准
- 人名/群名可解析；时间、关键词、发送人、消息类型可过滤。
- JSON 输出稳定、机器可读；`--help` / `tool-schema` / `call-json` 足以让通用 Agent 自发现，不强依赖额外 Skill。
- 大量导出可写 JSONL/Markdown，不需要把全量消息塞进模型上下文。
- 多 shard 不静默遗漏；未知 subtype/base type 不静默删除。
- 媒体缺失必须输出明确 warning/status，而不是把对应消息行丢掉。
