# 微信 Chat Exporter 第一阶段 ExecPlan

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

## 未来阶段（本轮不实施）
- 第二阶段：DB/WAL 增量 tail，形成稳定实时接收。
- 第三阶段：独立 send adapter（UI Automation/其它可行路线）。
- 第四阶段：家庭服务器托管微信/QQ + 自建 Web UI。

## 通过标准
- 人名/群名可解析；时间、关键词、发送人、消息类型可过滤。
- JSON 输出稳定、机器可读；`--help` / `tool-schema` / `call-json` 足以让通用 Agent 自发现，不强依赖额外 Skill。
- 大量导出可写 JSONL/Markdown，不需要把全量消息塞进模型上下文。
- 多 shard 不静默遗漏；未知 subtype/base type 不静默删除。
- 媒体缺失必须输出明确 warning/status，而不是把对应消息行丢掉。
