# 语音消息功能设计

日期：2026-10-09

## 背景与目标

当前机器人（`examples/receive-and-send`）已支持文字对话：固定 FAQ 命中优先、未命中走百炼大模型（`qwen-flash`），私聊/群聊@ 场景接入了基于 Redis 的最近 10 轮对话记忆，频道场景暂不接记忆。

目标：在现有文字能力基础上新增语音消息支持，覆盖两个方向：
1. **用户发语音 → 机器人能听懂**（ASR，私聊/群聊@/频道都支持）
2. **机器人用语音回复**（TTS，仅私聊/群聊@支持，原因见下）

## 范围确认

- 用户发语音时：解码 + ASR 转文字后，**接入现有文字处理流程**（FAQ → 记忆 → 百炼），不单独设计一套语音专用回复逻辑。
- 机器人回复语音的时机：**仅当本轮用户输入是语音时才回语音**；用户发文字，机器人始终回文字。不做"所有回复都带语音"或"关键词触发"。
- 场景覆盖：
  - 私聊 + 群聊@：支持双向语音（收语音、可回语音）。
  - 频道：**仅支持收语音（ASR转文字），回复始终用文字**。原因是 QQ 官方 Bot API 在频道场景没有提供富媒体语音发送接口（`openapi/v1/resource.go` 里只有 `c2cRichMediaURI`/`groupRichMediaURI`，没有频道等价物），这是 API 层硬限制，不是设计取舍。
- 长文本处理：大模型回复可能很长，TTS 前按配置的字数上限截断，避免生成超长语音消息拖慢链路/推高成本。

## 技术选型

> **2026-10-09 修订**：原方案 ASR/TTS 都用阿里云百炼。进入实现计划阶段调研 API 细节后发现百炼的录音文件识别是**异步任务模式**（提交任务→轮询，官方文档写"通常在数分钟内"完成），而当前 `webhook.HTTPHandler` 是同步阻塞调用整条处理链路直到发完回复才给 QQ 的 webhook 推送返回 ack——分钟级延迟会导致 (1) QQ webhook 投递判定超时重试、(2) 为了异步化引入的后台 goroutine 在 SCF 容器响应后可能被冻结/回收导致任务跑不完、(3) QQ 被动消息的回复时效窗口可能被错过。三个问题叠加，异步轮询模式在当前单体 SCF Web 函数部署下不可靠。因此 **ASR 改用腾讯云"一句话识别"（`SentenceRecognition`，同步 REST，1-2 秒返回）**，完全规避这三个问题，且该接口 `VoiceFormat` 原生支持 `silk`，不需要本地解码，可以把 QQ 语音附件原始字节直接 base64 后请求。TTS 暂时维持用阿里云百炼（CosyVoice 系列同步返回，没有上述异步轮询问题），是否统一换成腾讯云 TTS 留给 Plan B（语音回复）阶段再评估。

- **ASR**：腾讯云"一句话识别"（`asr.tencentcloudapi.com`，`Action=SentenceRecognition`，`Version=2019-06-14`）。同步 REST，`SourceType=1` 直传 `Data`（base64 音频）+ `DataLen`，`VoiceFormat=silk` 直接吃 QQ 语音附件原始字节，不需要本地解码。限制：单次音频 ≤3MB、≤60s（官方文档限制，QQ 语音消息正常场景下远小于这个上限）。签名用腾讯云官方 Go SDK（`tencentcloud-sdk-go/tencentcloud/asr/v20190614`），不手写 TC3-HMAC-SHA256。
- **TTS**：阿里云百炼 DashScope（CosyVoice 系列），与现有大模型调用共用账号体系。
- **Silk 编解码（仅 TTS 编码侧需要，Plan B 范围）**：CGO 直接链接 C silk 编解码库源码（vendor 进 `third_party/silk/`），产物仍是单一静态二进制，维持现有"一个二进制丢进 SCF Custom Runtime"的部署形态。因为 ASR 侧腾讯云接口直接吃 silk 格式，**Plan A（语音输入/ASR）完全不需要 CGO**，CGO 构建链变更推迟到 Plan B（语音输出/TTS）才引入。
  - 已评估并排除的替代方案：
    - `silk-wasm`（npm 包，Emscripten + embind 编译）：依赖 Emscripten 生成的 JS glue 代码做对象编组，无法直接作为裸 wasm 模块被 Go 的 wasm runtime（wazero/wasmer-go）调用；唯一可行路径是内嵌整个 Node.js 运行时，比直接 CGO 链接更重，淘汰。
    - 预编译 C 二进制 + `os/exec` 调用：可行，但需要在部署包里额外管理两个可执行文件的权限/路径，且构建流程不统一；作为备选记录，非首选。
    - 拆成独立服务（跨语言跑语音处理）：职责分离更清晰，但对当前 demo 阶段的工程规模是过度设计，引入多一套 SCF 函数部署/配置/固定出口 IP 管理的复杂度。
- **临时文件托管（仅 TTS 输出侧需要，Plan B 范围）**：腾讯云 COS。QQ 富媒体上传接口（`RichMediaMessage.URL`）要求传一个公网可访问的 HTTP/HTTPS 链接供 QQ 服务器回调拉取，不能直接传字节内容，因此 TTS 生成的语音文件必须先有一个公开 URL。ASR 侧不需要 COS（直传字节给腾讯云接口）。

## 整体架构

```
                    ┌─────────────────────────────────────┐
                    │           qqbot-svr (单体)            │
                    │                                       │
用户语音/文字 ──▶ process.go ──▶ [语音入口]asr.go ──▶ 文本    │
                    │               (腾讯云SentenceRecognition,│
                    │                直传silk字节,同步,无CGO)   │
                    │                 ▼                      │
                    │        generateReplyMessage            │
                    │        (FAQ / 百炼Chat，不变)            │
                    │                 │                      │
                    │                 ▼                      │
                    │   来源=语音且场景支持回语音?  [Plan B]      │
                    │     是 → tts.go → silk.go(CGO编码)       │
                    │          → cos.go(上传拿URL)             │
                    │          → 富媒体两步发送                 │
                    │     否 → 现有文字发送路径（不变）            │
                    └─────────────────────────────────────┘
```

核心原则：新增语音能力是现有文字流水线的"外壳"，不改动 `generateReplyMessage` 内部的 FAQ / 百炼 / 记忆逻辑。语音只做两件事——入口把语音转成文本喂给现有流程，出口把现有流程产出的文本转成语音发出去。

新增模块，各自职责单一：
- `asr.go`（**Plan A**） —— 调腾讯云一句话识别，输入 silk 字节直接输出文本，同步 REST，不涉及 CGO/COS
- `tts.go`（Plan B） —— 调百炼语音合成，输入文本输出音频字节
- `silk.go`（Plan B） —— CGO 封装，只管把 TTS 产出的 pcm/wav 编码成 silk，不关心 ASR/TTS/COS
- `cos.go`（Plan B） —— 上传临时文件到腾讯云 COS，返回公开可访问 URL（带过期策略）

## 数据流

### 私聊 / 群聊@（支持双向语音）

```
收到消息
  └─ 有 attachment 且 ContentType == "voice"？
       ├─ 是：下载附件URL拿到silk字节（原始字节，不解码）
       │      → asr.Transcribe(silkBytes) 调腾讯云SentenceRecognition(VoiceFormat=silk) → 文本
       │      ├─ ASR失败/超限(>3MB或>60s)/返回空 → 直接回复文字「没听清，可以再说一次或打字吗」，流程结束，不进大模型
       │      └─ 成功 → 得到 userText，标记 fromVoice=true，继续往下走
       └─ 否：userText = 原来的 input（纯文字），fromVoice=false

  → generateReplyMessage(userText, memKey)   // 原逻辑不变：先查FAQ，未命中再查记忆+调百炼
      得到 replyText

  → 写对话记忆时存文本（userText / replyText），跟语音无关，复用现有 AppendRound

  → fromVoice == true？
       ├─ 是：truncated := 按字数上限截断(replyText)
       │      → tts.Synthesize(truncated) 调百炼TTS → wav/pcm
       │      ├─ 失败 → 降级：走现有文字发送路径发 replyText
       │      └─ 成功 → silk.Encode(CGO) → cos.Upload() 拿临时URL
       │           ├─ COS上传失败 → 降级：走现有文字发送路径
       │           └─ 成功 → RichMediaMessage上传(拿file_info) → MessageToCreate{MsgType:7} 发送
       │                ├─ 任一步QQ API报错 → 降级：走现有文字发送路径
       │                └─ 成功 → 结束
       └─ 否：走现有文字发送路径（完全不变）
```

### 频道（只收不发）

```
收到消息 → 有voice attachment？
  ├─ 是：下载silk字节 → asr.Transcribe() → 失败则回「没听清」
  └─ 否：原文字

→ generateReplyMessage(...)  // 频道场景现在也没接记忆，memKey 继续传 ""
→ 始终走现有 sendChannelReply 文字发送路径，不管输入是不是语音
```

### 关键点

- `fromVoice` 标记只影响"要不要 TTS"，不影响大模型/FAQ 逻辑——语音转出来的文本和手敲的文本在 `generateReplyMessage` 眼里完全一样。
- 降级原则：任何语音相关环节出错，都退回已验证过的文字发送路径，不会因为语音功能的新 bug 导致用户收不到回复。
- 记忆里永远存文本，不存音频，跟现有"Redis 存最近10轮"的设计完全兼容，不用改 `memory.go`。

## 错误处理

延续项目现有风格（带前缀的日志如 `[Redis] 连接成功/失败`；配置缺失时静默降级而不是报错崩溃）。

| 环节 | 失败场景 | 处理方式 |
|---|---|---|
| 下载语音附件 | URL 不可达/超时 | 回复「语音好像没收到，可以再发一次吗」，不进 ASR/大模型 |
| ASR 调用（Plan A） | 接口报错/超时(建议 10s ctx)/音频超限(>3MB或>60s)/返回空文本 | 回复「没听清，可以再说一次或打字告诉我吗」；不占用/不写对话记忆（跟 FAQ 命中不占记忆是同一原则） |
| 后续 FAQ/百炼/记忆 | 复用现有逻辑 | 现有错误处理不变（`genErrMessage` 兜底） |
| TTS 调用（Plan B） | 接口报错/超时(10s ctx) | 降级为文字回复，日志 `[Voice] TTS失败，降级为文字: %v` |
| silk 编码（Plan B，CGO） | 编码异常 | 同上，降级文字 |
| COS 上传（Plan B） | 鉴权失败/网络错误 | 同上，降级文字 |
| QQ 富媒体两步发送（Plan B） | 上传/发送任一步报错 | 同上，降级文字；复用已经生成好的 `replyText`，不用重新调大模型 |

兜底原则：语音链路任何一步出问题，最终结果退化成"至少收到文字回复"，不会出现用户什么都没收到的情况。

## 配置与部署影响

### Plan A（ASR，语音输入）

**`config.yaml` 新增字段**（不进 git，留空则该功能自动关闭，跟现有 FAQ/Redis 的降级模式一致）：
- `tencent_secret_id` / `tencent_secret_key`：腾讯云 API 密钥（跟 SCF/COS 可以是同一账号下的密钥，但 ASR 用的是 API 密钥体系，不是 SCF 的角色权限，需要单独在腾讯云控制台开"语音识别"服务的访问权限）
- `tencent_asr_region`：如 `ap-guangzhou`
- `tencent_asr_eng_service_type`：识别引擎模型，如 `16k_zh`

**构建流程不变**：Plan A 不涉及 CGO，继续用现有 `make -f Makefile build` 纯 Go 交叉编译即可。

**新依赖**：引入官方 `github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/asr/v20190614`（含 `common` 子包做签名），`examples/go.mod` 跑 `go mod tidy`（**`examples/` 是独立 go module**，之前接 Redis 时在这里踩过一次 go.sum 缺条目的坑）。

**SCF 执行时限**：ASR 同步调用 1-2 秒量级，叠加下载附件和现有百炼调用，影响很小，现有超时配置应该够用，不强制要求调整，实现时实测确认一下。

### Plan B（TTS，语音输出，本次不实现，记录供下一轮设计参考）

- `config.yaml` 还需要 `bailian_tts_model`、`cos_secret_id`/`cos_secret_key`/`cos_region`/`cos_bucket`、`voice_reply_max_chars`。
- 构建流程要从纯 Go 交叉编译换成 CGO（推荐 `zig cc -target x86_64-linux-musl` 静态链接，避免 SCF 上 glibc 版本不兼容），Makefile 加 `CGO_ENABLED=1`+`CC=`参数，silk 源码 vendor 进 `third_party/silk/`。
- COS 上传引入 `tencentyun/cos-go-sdk-v5`。
- SCF 执行超时需要调到 30-60s（下载+TTS+编码+COS上传+两步发送比 Plan A 多好几秒）。

## 不在本次范围内

- 频道场景的语音回复（API 不支持）。
- 实时语音通话（频道语音子频道场景），本设计只覆盖"发语音消息"，不是"打电话"。
- 语音音色/情感等精细化 TTS 参数调优，先用默认音色跑通链路。
- **Plan B（TTS 语音回复）本身**：本设计文档覆盖了完整方案，但实现计划拆成两份——先交付 Plan A（ASR 语音输入），Plan B 等 Plan A 上线验证后再单独写实现计划。到时候需要重新评估 TTS 是否也换成腾讯云（现在 ASR 已经引入腾讯云账号体系，TTS 继续用百炼还是统一换腾讯云，留到那时候再决定，不在本次讨论范围）。
