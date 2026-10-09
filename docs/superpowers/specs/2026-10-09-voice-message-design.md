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

- **ASR / TTS**：阿里云百炼 DashScope，与现有大模型调用共用同一账号体系/API Key 管理方式，不额外引入新云厂商的认证体系。
- **Silk 编解码**：CGO 直接链接 C silk 编解码库源码（vendor 进 `third_party/silk/`），产物仍是单一静态二进制，维持现有"一个二进制丢进 SCF Custom Runtime"的部署形态。
  - 已评估并排除的替代方案：
    - `silk-wasm`（npm 包，Emscripten + embind 编译）：依赖 Emscripten 生成的 JS glue 代码做对象编组，无法直接作为裸 wasm 模块被 Go 的 wasm runtime（wazero/wasmer-go）调用；唯一可行路径是内嵌整个 Node.js 运行时，比直接 CGO 链接更重，淘汰。
    - 预编译 C 二进制 + `os/exec` 调用：可行，但需要在部署包里额外管理两个可执行文件的权限/路径，且构建流程不统一；作为备选记录，非首选。
    - 拆成独立服务（跨语言跑语音处理）：职责分离更清晰，但对当前 demo 阶段的工程规模是过度设计，引入多一套 SCF 函数部署/配置/固定出口 IP 管理的复杂度。
- **临时文件托管**：腾讯云 COS。QQ 富媒体上传接口（`RichMediaMessage.URL`）要求传一个公网可访问的 HTTP/HTTPS 链接供 QQ 服务器回调拉取，不能直接传字节内容，因此生成的语音文件必须先有一个公开 URL。COS 与现有 SCF 同生态，设置短期有效期/生命周期规则即可。

## 整体架构

```
                    ┌─────────────────────────────────────┐
                    │           qqbot-svr (单体)            │
                    │                                       │
用户语音/文字 ──▶ process.go ──▶ [语音入口]asr.go ──▶ 文本    │
                    │                 │ (silk解码,CGO)        │
                    │                 ▼                      │
                    │        generateReplyMessage            │
                    │        (FAQ / 百炼Chat，不变)            │
                    │                 │                      │
                    │                 ▼                      │
                    │   来源=语音且场景支持回语音?               │
                    │     是 → tts.go → silk.go(CGO编码)       │
                    │          → cos.go(上传拿URL)             │
                    │          → 富媒体两步发送                 │
                    │     否 → 现有文字发送路径（不变）            │
                    └─────────────────────────────────────┘
```

核心原则：新增语音能力是现有文字流水线的"外壳"，不改动 `generateReplyMessage` 内部的 FAQ / 百炼 / 记忆逻辑。语音只做两件事——入口把语音转成文本喂给现有流程，出口把现有流程产出的文本转成语音发出去。

新增模块，各自职责单一：
- `silk.go` —— CGO 封装，只管 silk ⇄ pcm/wav 的编解码，不关心 ASR/TTS/COS
- `asr.go` —— 调百炼语音识别，输入音频字节输出文本
- `tts.go` —— 调百炼语音合成，输入文本输出音频字节
- `cos.go` —— 上传临时文件到腾讯云 COS，返回公开可访问 URL（带过期策略）

## 数据流

### 私聊 / 群聊@（支持双向语音）

```
收到消息
  └─ 有 attachment 且 ContentType == "voice"？
       ├─ 是：下载附件URL → silk.Decode(CGO) → pcm/wav
       │      → asr.Transcribe() 调百炼ASR → 文本
       │      ├─ ASR失败/返回空 → 直接回复文字「没听清，可以再说一次或打字吗」，流程结束，不进大模型
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
  ├─ 是：下载 → silk解码 → ASR转文字 → 失败则回「没听清」
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
| silk 解码（CGO） | 非标准 silk / 解码异常 | 同上文案；记录 `[Voice] silk解码失败: %v` |
| ASR 调用 | 接口报错/超时(10s ctx)/返回空文本 | 回复「没听清，可以再说一次或打字告诉我吗」；不占用/不写对话记忆（跟 FAQ 命中不占记忆是同一原则） |
| 后续 FAQ/百炼/记忆 | 复用现有逻辑 | 现有错误处理不变（`genErrMessage` 兜底） |
| TTS 调用 | 接口报错/超时(10s ctx) | 降级为文字回复，日志 `[Voice] TTS失败，降级为文字: %v` |
| silk 编码（CGO） | 编码异常 | 同上，降级文字 |
| COS 上传 | 鉴权失败/网络错误 | 同上，降级文字 |
| QQ 富媒体两步发送 | 上传/发送任一步报错 | 同上，降级文字；复用已经生成好的 `replyText`，不用重新调大模型 |

兜底原则：语音链路任何一步出问题，最终结果退化成"至少收到文字回复"，不会出现用户什么都没收到的情况。

## 配置与部署影响

**`config.yaml` 新增字段**（不进 git，缺哪个就自动关哪个功能，跟现有 FAQ/Redis 的降级模式一致）：
- `bailian_asr_model` / `bailian_tts_model`：复用现有 `bailian_api_key`
- `cos_secret_id` / `cos_secret_key` / `cos_region` / `cos_bucket`：留空则整个语音回复功能关闭，自动退化为纯文字
- `voice_reply_max_chars`：TTS 前的截断字数上限

**构建流程变化（有破坏性）**：
- 现有 `make -f Makefile build` 是纯 Go 交叉编译（`GOOS=linux GOARCH=amd64 go build`）。CGO 链接 C silk 编解码库后必须换成带 C 交叉编译器的构建，推荐用 `zig cc -target x86_64-linux-musl` 做**静态链接**，产出不依赖目标机器 glibc 版本的二进制——否则容易出现"本地编译能跑、SCF 上报 `GLIBC_X.XX not found`"。Makefile 要加 `CGO_ENABLED=1` 和对应 `CC=` 参数。
- silk 的 C 源码 vendor 进 `third_party/silk/`，Go 侧用 `import "C"` + cgo `CFLAGS`/`LDFLAGS` 指向该目录。

**SCF 执行时限核对**：语音链路（下载+ASR+TTS+编码+COS上传+两步发送）比纯文字多好几秒，需要去 SCF 控制台确认"执行超时时间"是否够用（现有百炼调用用了 15s ctx timeout），建议语音链路把函数超时调到 30-60s。

**新依赖**：
- ASR/TTS 照抄 `bailian.go` 风格，手写 `net/http` 调 DashScope 兼容接口，不引入额外 SDK。
- COS 上传引入官方 `tencentyun/cos-go-sdk-v5`，`examples/go.mod` 跑 `go mod tidy`（**`examples/` 是独立 go module**，之前接 Redis 时在这里踩过一次 go.sum 缺条目的坑）。

## 不在本次范围内

- 频道场景的语音回复（API 不支持）。
- 实时语音通话（频道语音子频道场景），本设计只覆盖"发语音消息"，不是"打电话"。
- 语音音色/情感等精细化 TTS 参数调优，先用默认音色跑通链路。
