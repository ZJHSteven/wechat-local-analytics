# 项目状态快照

## 当前结论（必须最新）
- 现状：`wechat-cli` 1.6.20 已在 Windows 微信 4.1.9.57 + Sandboxie 环境完成 DB key、会话解析、timeline、search、JSONL/Markdown 导出的真实数据验证。
- 已完成：找到 r266 幸存源码；Windows 编译成功；构建 SQLCipher 兼容 `libWCDB.dll`；首次 `cache refresh --force` 成功获取 24 个 DB key；仝立炎已知窗口返回精确 73 条。
- 正在做：基于用户刚从手机回灌后的更完整历史，重新建立全库回归基线；调查微信原生语音转写缓存。回灌后 cache 已从 24 个 DB key / 855 会话增长到 26 个 DB key / 1238 会话。
- 图片 key 根因已定位：幸存源码的 Windows DB key 已内置扫描，但 `RunImageKey` 仍硬依赖原 release 的 `wxkey.exe`；DMCA 后该 helper 缺失，导致每次图片读取都只留下 `image_decode_needs_image_key`。现已加入 Windows 进程内图片 key 扫描实现，待真实图片回归验证。
- 下一步：运行 CLI 回归测试、全库类型扫描、重点媒体/复杂消息抽样；只修影响 AI 完整理解的缺口。

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
