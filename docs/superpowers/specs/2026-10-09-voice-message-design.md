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

- **ASR**：腾讯云"一句话识别"（`asr.tencentcloudapi.com`，`Action=SentenceRecognition`，`Version=2019-06-14`）。同步 REST，`SourceType=1` 直传 `Data`（base64 音频）+ `DataLen`，`VoiceFormat=silk` 直接吃 QQ 语音附件原始字节，不需要本地解码。限制：单次音频 ≤3MB、≤60s（官方文档限制，QQ 语音消息正常场景下远小于这个上限）。签名用腾讯云官方 Go SDK（`tencentcloud-sdk-go/tencentcloud/asr/v20190614`），不手写 TC3-HMAC-SHA256。**2026-10-09 本地真实语音消息端到端验证通过**，详见下方"Plan A 验证记录"。

> **2026-10-09 Plan B 补充修订**：Plan B 设计阶段时把 TTS 也从阿里云百炼改成了腾讯云，并且找到一个比 CGO 更简单的 silk 编码方案，具体见下。

- **TTS**：**腾讯云"基础语音合成"**（`tts.tencentcloudapi.com`，`Action=TextToVoice`，`Version=2019-08-23`），而不是原计划的阿里云百炼。原因：(1) 跟 ASR 用同一套腾讯云账号/密钥，不用再接一套阿里云的认证；(2) 调研发现百炼 CosyVoice 的"非流式"合成底层仍然是 WebSocket（SDK 内部维护长连接，没有纯 HTTP REST 调用方式），接入复杂度明显高于腾讯云这边——腾讯云是纯同步 HTTP POST，直接在响应体里拿 base64 音频（`Response.Audio`），没有百炼那种协议复杂度。硬约束：**单次最多 150 个汉字**（全角标点算一个字），超过要截断，更长的文本只能走"长文本语音合成"（异步任务），会重蹈百炼 ASR 异步模式的覆辙，所以不用。输出格式用 `Codec=pcm`、`SampleRate=24000`（这个采样率是部分音色才支持，要在音色列表里选对应音色），直接拿到适合喂给 silk 编码器的裸 PCM，不用处理 wav 头。
- **Silk 编码（仅 TTS 输出侧需要，Plan B 范围）**：**纯 Go 库 `github.com/youthlin/silk`**（从 silk v1.0.9 的 C 版本移植），直接 `Encode(pcmReader, silk.SampleRate(24000))` 拿到 silk v3 字节，**不需要 CGO**，继续维持 `CGO_ENABLED=0` 纯 Go 交叉编译，这是 Plan B 设计阶段相对最初方案最大的简化——原计划是 CGO 链接 C silk 库，需要换 `zig cc` 交叉编译链。**2026-10-10 实现阶段修正**：这个库的 `Stx` 选项默认值其实是 `false`（Go 零值），不是设计阶段最初以为的"默认开启"——`Encode` 必须显式传 `silk.Stx(true)` 才会在 "#!SILK_V3" 头前加上 QQ/微信要求的 0x02(STX) 前缀字节，否则 QQ 会把文件当成普通附件而不是语音消息处理。这一点是 Task 2 的 task reviewer 读库源码发现的，已在 `silk.go` 里修正并通过复审。
  - 风险：这个库相对小众（15 star、12 次提交），不如 `kn007/silk-v3-decoder`（微信/QQ生态里最广泛使用的 C 实现）久经考验，需要先用真实 QQ 账号实测验证编码出来的语音能不能正常播放，而不是只看单测通过；如果验证发现编码质量有问题，回退到 CGO 链接 `kn007/silk-v3-decoder` 这个备选方案。
  - 仍然评估并排除的替代方案（跟 Plan A 阶段的结论一致，没有变化）：
    - `silk-wasm`（npm 包，Emscripten + embind 编译）：依赖 JS glue 代码，裸 wasm 模块塞不进 Go 的 wasm runtime，淘汰。
    - 拆成独立服务（跨语言跑语音处理）：对当前 demo 阶段规模是过度设计。
- **临时文件托管（仅 TTS 输出侧需要，Plan B 范围）**：腾讯云 COS。QQ 富媒体上传接口（`RichMediaMessage.URL`）要求传一个公网可访问的 HTTP/HTTPS 链接供 QQ 服务器回调拉取，不能直接传字节内容，因此 TTS 生成的语音文件必须先有一个公开 URL。用**预签名 URL**（有效期约10分钟），不把 bucket 设成公开读，权限上更收敛。ASR 侧不需要 COS（直传字节给腾讯云接口）。

## Plan A 验证记录

2026-10-09，本地起 ngrok + 本地跑 `qqbot-svr`，把 QQ 开放平台回调地址临时指过来，私聊机器人发了一条真实语音消息。日志里 `[Voice] 语音识别结果: 啊你好你好啊你好啊。`，跟 QQ 回调 payload 里自带的参考字段 `asr_refer_text` 完全一致，证明识别准确。顺带发现两个跟语音功能无关的已知限制（本地出口 IP 不在 QQ 白名单、本地网络连百炼超时），都不是本次功能的缺陷。

## 整体架构

```
                    ┌──────────────────────────────────────────┐
                    │              qqbot-svr (单体)               │
                    │                                            │
用户语音/文字 ──▶ process.go ──▶ [语音入口]asr.go ──▶ 文本         │
                    │               (腾讯云SentenceRecognition,   │
                    │                直传silk字节,同步,无CGO)      │
                    │                 ▼                          │
                    │        generateReplyMessage                │
                    │        (FAQ / 百炼Chat，不变)                │
                    │                 │                          │
                    │                 ▼                          │
                    │   fromVoice==true 且 scene支持回语音?[PlanB] │
                    │     是 → sendVoiceReply(ctx,scene,...)      │
                    │          ├─ 截断到150字(超限加提示语)         │
                    │          ├─ tts.go: 腾讯云TTS → PCM          │
                    │          ├─ silk.go: 纯Go库编码 → silk字节   │
                    │          ├─ cos.go: 上传COS → 预签名URL      │
                    │          └─ 富媒体两步发送                   │
                    │          任一步失败 → 返回err                │
                    │     否/失败 → 现有文字发送路径（不变）         │
                    └──────────────────────────────────────────┘
```

核心原则：新增语音能力是现有文字流水线的"外壳"，不改动 `generateReplyMessage` 内部的 FAQ / 百炼 / 记忆逻辑。语音只做两件事——入口把语音转成文本喂给现有流程，出口把现有流程产出的文本转成语音发出去。

新增模块，各自职责单一：
- `asr.go`（**Plan A，已实现**） —— 调腾讯云一句话识别，输入 silk 字节直接输出文本，同步 REST，不涉及 CGO/COS。`resolveUserInput` 额外返回一个 `fromVoice bool`，标记"这轮是不是语音输入且识别成功"，供 Plan B 的出口判断是否要尝试语音回复。
- `tts.go`（Plan B） —— 调腾讯云语音合成，输入文本输出 PCM 字节
- `silk.go`（Plan B） —— 薄封装纯 Go 库 `youthlin/silk`，PCM → silk v3，不涉及 CGO
- `cos.go`（Plan B） —— 上传临时文件到腾讯云 COS，返回预签名 URL（~10分钟时效）
- 编排入口 `sendVoiceReply(ctx, scene, targetID, text, refMsgID) error`（Plan B，暂定放在新文件 `voice_reply.go`）—— 串起 TTS→编码→上传→两步发送整条链，任何一步失败就返回 error，调用方统一降级发文字

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
      得到 msg（*dto.MessageToCreate）

  → 写对话记忆时存文本（userText / msg.Content），跟语音无关，复用现有 AppendRound

  → fromVoice == true？
       ├─ 是：err := sendVoiceReply(ctx, scene, targetID, msg.Content, data.ID)
       │        内部：截断到150字(超限加提示语) → tts.Synthesize → pcm
       │              → EncodeToSilk(pcm) → silk字节
       │              → cos.UploadAndSign() → 预签名URL
       │              → RichMediaMessage上传(拿file_info) → MessageToCreate{MsgType:7} 两步发送
       │        任一步失败 → 返回wrapped error
       │      ├─ err == nil → 结束（语音已发出）
       │      └─ err != nil → log「[Voice] 语音回复失败，降级为文字」→ 走现有文字发送路径发 msg
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
| TTS/COS 未配置（Plan B） | `tencent_tts_voice_type`/`cos_bucket` 留空 | `sendVoiceReply` 一开始就检查，直接返回错误，日志 `[Voice] TTS/COS未配置，降级为文字` |
| TTS 调用（Plan B） | 接口报错/超时(建议10s ctx)/返回空音频 | 返回错误，日志 `[Voice] TTS调用失败: %v`，调用方降级发文字 |
| silk 编码（Plan B，纯Go库） | 编码异常 | 同上，日志 `[Voice] silk编码失败: %v` |
| COS 上传（Plan B） | 鉴权失败/网络错误/bucket不存在 | 同上，日志 `[Voice] COS上传失败: %v` |
| QQ 富媒体两步发送（Plan B） | 上传/发送任一步报错，或上传响应没有`FileInfo` | 同上；复用已经生成好的 `msg.Content`，不用重新调大模型 |

兜底原则：语音链路任何一步出问题，最终结果退化成"至少收到文字回复"，不会出现用户什么都没收到的情况。`sendVoiceReply` 内部不区分失败原因分别处理，统一返回 error 由调用方走同一条降级路径——出错原因记日志，不需要调用方按失败类型分支。

超时预算：TTS（10s）+ COS 上传（建议5s）+ 两次 QQ API 调用，整条链路预计 15-20 秒，比 Plan A 的 ASR 链路（10s 级）长，部署前需要核对 SCF 执行超时配置。

## 配置与部署影响

### Plan A（ASR，语音输入）

**`config.yaml` 新增字段**（不进 git，留空则该功能自动关闭，跟现有 FAQ/Redis 的降级模式一致）：
- `tencent_secret_id` / `tencent_secret_key`：腾讯云 API 密钥（跟 SCF/COS 可以是同一账号下的密钥，但 ASR 用的是 API 密钥体系，不是 SCF 的角色权限，需要单独在腾讯云控制台开"语音识别"服务的访问权限）
- `tencent_asr_region`：如 `ap-guangzhou`
- `tencent_asr_eng_service_type`：识别引擎模型，如 `16k_zh`

**构建流程不变**：Plan A 不涉及 CGO，继续用现有 `make -f Makefile build` 纯 Go 交叉编译即可。

**新依赖**：引入官方 `github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/asr/v20190614`（含 `common` 子包做签名），`examples/go.mod` 跑 `go mod tidy`（**`examples/` 是独立 go module**，之前接 Redis 时在这里踩过一次 go.sum 缺条目的坑）。

**SCF 执行时限**：ASR 同步调用 1-2 秒量级，叠加下载附件和现有百炼调用，影响很小，现有超时配置应该够用，不强制要求调整，实现时实测确认一下。

### Plan B（TTS，语音输出）

**`config.yaml` 新增字段**（留空即功能关闭，跟 Plan A 一致的降级模式）：
- `tencent_tts_voice_type`：腾讯云 TTS 音色 ID（整数），需要去音色列表选一个**支持 24000 采样率**的
- `cos_bucket` / `cos_region`：COS bucket 名和地域；留空则语音回复功能整体关闭（不影响 ASR）
- 复用 Plan A 已有的 `tencent_secret_id`/`tencent_secret_key`——COS、TTS、ASR 是同一个腾讯云账号下的 API 密钥，不用分开配置

**构建流程不变**：Plan B 最终选定纯 Go 的 `youthlin/silk` 做编码，**不需要 CGO**，继续 `CGO_ENABLED=0` 纯 Go 交叉编译，Makefile 不用改。（原计划的 CGO 链接 C silk 库方案作为备选保留，如果纯 Go 库实测编码质量有问题再切换。）

**COS 新建 bucket 步骤**（控制台手动操作）：
1. 对象存储 COS → 创建存储桶，地域建议跟 SCF/Redis 一致（`ap-guangzhou`），访问权限选**私有读写**（用预签名 URL 访问，不需要公开读）
2. 不需要配置 CDN/自定义域名
3. 可选：加一条生命周期规则（如1天后自动删除），防止临时语音文件堆积——不是必须，预签名 URL 本身~10分钟就过期

**新依赖**：
- `github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/tts/v20190823`（TTS，跟 ASR 同一个 SDK 仓库、同样的调用范式：`common.NewCredential`+`profile.NewClientProfile`+`tts.NewClient`+`TextToVoiceWithContext`）
- `github.com/youthlin/silk`（纯 Go silk 编解码，`Encode(pcmReader, silk.SampleRate(24000))`）
- `github.com/tencentyun/cos-go-sdk-v5`（COS 官方 SDK）

**SCF 执行超时**：需要核对是否已调到 30-60s（Plan A 部署时可能已经调整过，具体看当时设的值；Plan B 链路比 Plan A 长，需要确认够用）。

## 不在本次范围内

- 频道场景的语音回复（API 不支持）。
- 实时语音通话（频道语音子频道场景），本设计只覆盖"发语音消息"，不是"打电话"。
- 语音音色/情感等精细化 TTS 参数调优，先用默认音色跑通链路。
- 长文本语音合成（>150字走异步任务）——跟 Plan A 放弃百炼异步 ASR 的理由一样，不适合同步 webhook 模型，统一用截断代替。
- `youthlin/silk` 实测如果编码质量有问题，回退到 CGO 链接 `kn007/silk-v3-decoder` 的方案——这个决策留到实现/联调阶段再做，不在设计阶段预判。
