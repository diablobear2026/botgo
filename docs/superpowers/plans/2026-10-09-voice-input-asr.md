# 语音输入(ASR) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让机器人能听懂用户发来的语音消息——识别成文字后接入现有的 FAQ/百炼/对话记忆文字处理流程，私聊/群聊@/频道三个场景都支持；机器人仍然只用文字回复（语音回复是 Plan B，不在本计划范围）。

**Architecture:** 新增 `asr.go` 封装腾讯云"一句话识别"（`SentenceRecognition`，同步 REST，`VoiceFormat=silk` 直传 QQ 语音附件原始字节，不需要本地解码）。`process.go` 里三个 `Process*Message` 入口函数在走现有文字流程之前，先检查消息是否带语音附件；有则下载字节→调 ASR→把识别出的文字当作 `input` 继续走 `generateReplyMessage`（FAQ/百炼/记忆逻辑完全不改）；识别失败/未配置 ASR 则直接回复固定兜底文案，不进大模型。

**Tech Stack:** Go 1.21，`github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/asr/v20190614` 官方 SDK（负责 TC3-HMAC-SHA256 签名，不手写）。

## Global Constraints

- 腾讯云"一句话识别"单次音频限制：**≤3MB、≤60s**。超限由接口报错，走兜底文案分支，不用在本地提前精确校验时长。
- `VoiceFormat` 固定传 `"silk"`，QQ 语音附件原始字节直接 base64 后传给接口，**不做本地 silk 解码**。
- 本计划**不引入 CGO**，继续用现有 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build`（见 `examples/receive-and-send/Makefile`）。CGO 是 Plan B（TTS 语音回复）才需要的改动。
- `config.yaml` 新增配置项留空即功能关闭（降级为"不识别语音，提示用户打字"），不影响现有 FAQ/百炼/Redis 记忆功能，跟项目现有的降级哲学一致。
- 新代码风格跟现有文件（`bailian.go`/`memory.go`/`faq.go`）保持一致：中文日志、`fmt.Errorf("...: %w", err)` 包装错误、新日志统一带 `[Voice]` 前缀（参考现有 `[Redis]` 前缀的用法）。
- `examples/` 是独立的 go module（有自己的 `go.mod`），依赖变更要在 `examples/` 目录下执行 `go get`/`go mod tidy`，不是根目录 `botgo` module。
- 设计依据：`docs/superpowers/specs/2026-10-09-voice-message-design.md`（已根据实现调研修订为腾讯云同步 ASR 方案）。

---

### Task 1: 腾讯云 ASR 客户端与结果解析

**Files:**
- Create: `examples/receive-and-send/asr.go`
- Create: `examples/receive-and-send/asr_test.go`
- Modify: `examples/receive-and-send/go.mod`, `examples/receive-and-send/go.sum`（通过 `go get`/`go mod tidy` 自动更新，不手改）

**Interfaces:**
- Produces：
  - `type TencentASRClient struct{...}`
  - `func NewTencentASRClient(secretID, secretKey, region, engServiceType string) (*TencentASRClient, error)`
  - `func (c *TencentASRClient) Transcribe(ctx context.Context, audioBytes []byte) (string, error)`
  - `func extractRecognizedText(resp *asr.SentenceRecognitionResponse) (string, error)`（包内私有，供 Transcribe 内部使用，也是本任务单测的主要对象）

- [ ] **Step 1: 添加腾讯云 ASR SDK 依赖**

Run:
```bash
cd examples
go get github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/asr/v20190614@latest
go mod tidy
```
Expected: `go.mod` 新增一条 `github.com/tencentcloud/tencentcloud-sdk-go/...` 的 require（具体版本号以 `go get` 解析到的最新版为准），`go.sum` 相应增加校验记录，命令本身无报错退出。

- [ ] **Step 2: 写失败的单测（`extractRecognizedText`）**

创建 `examples/receive-and-send/asr_test.go`：

```go
package main

import (
	"testing"

	asr "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/asr/v20190614"
)

func strPtr(s string) *string { return &s }

func TestExtractRecognizedText(t *testing.T) {
	cases := []struct {
		name    string
		resp    *asr.SentenceRecognitionResponse
		want    string
		wantErr bool
	}{
		{name: "nil response", resp: nil, wantErr: true},
		{name: "nil Response字段", resp: &asr.SentenceRecognitionResponse{}, wantErr: true},
		{
			name:    "nil Result",
			resp:    &asr.SentenceRecognitionResponse{Response: &asr.SentenceRecognitionResponseParams{}},
			wantErr: true,
		},
		{
			name:    "空字符串Result",
			resp:    &asr.SentenceRecognitionResponse{Response: &asr.SentenceRecognitionResponseParams{Result: strPtr("")}},
			wantErr: true,
		},
		{
			name:    "只有空格的Result",
			resp:    &asr.SentenceRecognitionResponse{Response: &asr.SentenceRecognitionResponseParams{Result: strPtr("   ")}},
			wantErr: true,
		},
		{
			name: "正常识别结果(带前后空格需要trim)",
			resp: &asr.SentenceRecognitionResponse{
				Response: &asr.SentenceRecognitionResponseParams{Result: strPtr(" 今天天气怎么样 ")},
			},
			want: "今天天气怎么样",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := extractRecognizedText(c.resp)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望返回错误，实际返回 nil (text=%q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不期望报错，实际报错: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
```

- [ ] **Step 3: 运行测试，确认失败**

Run: `cd examples/receive-and-send && go test ./... -run TestExtractRecognizedText -v`
Expected: 编译失败，报 `undefined: extractRecognizedText`（因为 `asr.go` 还没创建）。

- [ ] **Step 4: 实现 `asr.go`**

创建 `examples/receive-and-send/asr.go`：

```go
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	asr "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/asr/v20190614"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
)

// TencentASRClient 封装对腾讯云"一句话识别"接口的调用
type TencentASRClient struct {
	client         *asr.Client
	engServiceType string
}

// NewTencentASRClient 创建一个腾讯云ASR客户端
func NewTencentASRClient(secretID, secretKey, region, engServiceType string) (*TencentASRClient, error) {
	credential := common.NewCredential(secretID, secretKey)
	cpf := profile.NewClientProfile()
	client, err := asr.NewClient(credential, region, cpf)
	if err != nil {
		return nil, fmt.Errorf("创建腾讯云ASR客户端失败: %w", err)
	}
	if engServiceType == "" {
		engServiceType = "16k_zh"
	}
	return &TencentASRClient{client: client, engServiceType: engServiceType}, nil
}

// Transcribe 调腾讯云一句话识别。audioBytes 是语音文件原始字节——QQ语音消息是silk格式，
// 腾讯云接口原生支持 VoiceFormat=silk，可以直接传，不需要本地解码。
func (c *TencentASRClient) Transcribe(ctx context.Context, audioBytes []byte) (string, error) {
	request := asr.NewSentenceRecognitionRequest()
	request.ProjectId = common.Uint64Ptr(0)
	request.SubServiceType = common.Uint64Ptr(2) // 2 = 一句话识别
	request.EngSerViceType = common.StringPtr(c.engServiceType)
	request.SourceType = common.Uint64Ptr(1) // 1 = 直传音频数据（非URL）
	request.VoiceFormat = common.StringPtr("silk")
	request.Data = common.StringPtr(base64.StdEncoding.EncodeToString(audioBytes))
	request.DataLen = common.Int64Ptr(int64(len(audioBytes)))

	response, err := c.client.SentenceRecognitionWithContext(ctx, request)
	if err != nil {
		return "", fmt.Errorf("调用腾讯云ASR接口失败: %w", err)
	}
	return extractRecognizedText(response)
}

// extractRecognizedText 从腾讯云ASR响应里提取识别出的文字，处理各种空值情况
func extractRecognizedText(resp *asr.SentenceRecognitionResponse) (string, error) {
	if resp == nil || resp.Response == nil || resp.Response.Result == nil {
		return "", errors.New("腾讯云ASR未返回识别结果")
	}
	text := strings.TrimSpace(*resp.Response.Result)
	if text == "" {
		return "", errors.New("腾讯云ASR识别结果为空")
	}
	return text, nil
}
```

- [ ] **Step 5: 运行测试，确认通过**

Run: `cd examples/receive-and-send && go test ./... -run TestExtractRecognizedText -v`
Expected: 全部 `PASS`。

- [ ] **Step 6: Commit**

```bash
cd examples/receive-and-send
git add asr.go asr_test.go go.mod go.sum
git commit -m "新增腾讯云ASR客户端，封装一句话识别接口调用与结果解析"
```

---

### Task 2: 语音附件查找、下载与识别入口

**Files:**
- Modify: `examples/receive-and-send/asr.go`（追加代码，不删除 Task 1 的内容）
- Modify: `examples/receive-and-send/asr_test.go`（追加测试）
- Modify: `examples/receive-and-send/process.go:15-20`（`Processor` 结构体加一个字段，仅此一处改动，三个 `Process*Message` 函数的改动在 Task 3）

**Interfaces:**
- Consumes：Task 1 的 `(*TencentASRClient).Transcribe(ctx context.Context, audioBytes []byte) (string, error)`
- Produces：
  - `type voiceTranscriber interface { Transcribe(ctx context.Context, audioBytes []byte) (string, error) }`
  - `func findVoiceAttachment(attachments []*dto.MessageAttachment) *dto.MessageAttachment`
  - `const maxVoiceBytes = 3 * 1024 * 1024`
  - `func downloadVoiceBytes(ctx context.Context, url string) ([]byte, error)`
  - `func (p Processor) resolveUserInput(ctx context.Context, input string, attachments []*dto.MessageAttachment) (resolved string, needFallback bool)`
  - `func genVoiceFallbackMessage(data dto.Message) *dto.MessageToCreate`
  - `Processor` 结构体新增字段 `asrClient voiceTranscriber`

- [ ] **Step 1: 写失败的单测（`findVoiceAttachment` / `downloadVoiceBytes`）**

在 `examples/receive-and-send/asr_test.go` 追加（文件顶部 `import` 块需要同步加上 `bytes`、`context`、`net/http`、`net/http/httptest`、`"github.com/tencent-connect/botgo/dto"`）：

```go
func TestFindVoiceAttachment(t *testing.T) {
	voice := &dto.MessageAttachment{ContentType: "voice", URL: "https://example.com/a.silk"}
	image := &dto.MessageAttachment{ContentType: "image/png", URL: "https://example.com/b.png"}

	cases := []struct {
		name        string
		attachments []*dto.MessageAttachment
		want        *dto.MessageAttachment
	}{
		{name: "nil切片", attachments: nil, want: nil},
		{name: "空切片", attachments: []*dto.MessageAttachment{}, want: nil},
		{name: "没有语音附件", attachments: []*dto.MessageAttachment{image}, want: nil},
		{name: "语音附件混在其他附件里", attachments: []*dto.MessageAttachment{image, voice}, want: voice},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := findVoiceAttachment(c.attachments)
			if got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestDownloadVoiceBytes(t *testing.T) {
	t.Run("下载成功", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("fake-silk-bytes"))
		}))
		defer srv.Close()

		data, err := downloadVoiceBytes(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("不期望报错: %v", err)
		}
		if string(data) != "fake-silk-bytes" {
			t.Fatalf("got %q", string(data))
		}
	})

	t.Run("非200状态码", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		if _, err := downloadVoiceBytes(context.Background(), srv.URL); err == nil {
			t.Fatal("期望报错，实际没有报错")
		}
	})

	t.Run("超过大小限制", func(t *testing.T) {
		big := bytes.Repeat([]byte("a"), maxVoiceBytes+1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(big)
		}))
		defer srv.Close()

		if _, err := downloadVoiceBytes(context.Background(), srv.URL); err == nil {
			t.Fatal("期望报错，实际没有报错")
		}
	})
}
```

- [ ] **Step 2: 运行测试，确认失败**

Run: `cd examples/receive-and-send && go test ./... -run 'TestFindVoiceAttachment|TestDownloadVoiceBytes' -v`
Expected: 编译失败，报 `undefined: findVoiceAttachment` / `undefined: downloadVoiceBytes` / `undefined: maxVoiceBytes`。

- [ ] **Step 3: 在 `asr.go` 追加实现**

在 `examples/receive-and-send/asr.go` 文件末尾追加（并把 `import` 块里加上 `io`、`log`、`net/http`、`time`、`"github.com/tencent-connect/botgo/dto"`）：

```go
// maxVoiceBytes 腾讯云一句话识别单次音频大小限制为3MB，提前拒绝超限文件，不浪费一次无意义的接口调用
const maxVoiceBytes = 3 * 1024 * 1024

// voiceTranscriber 抽象ASR客户端能力，方便单测时用假实现替换真实的腾讯云调用
type voiceTranscriber interface {
	Transcribe(ctx context.Context, audioBytes []byte) (string, error)
}

// findVoiceAttachment 从消息附件里找出第一个语音附件，没有则返回nil
func findVoiceAttachment(attachments []*dto.MessageAttachment) *dto.MessageAttachment {
	for _, a := range attachments {
		if a != nil && a.ContentType == "voice" {
			return a
		}
	}
	return nil
}

// downloadVoiceBytes 下载语音附件的原始字节（QQ语音消息是silk格式），超过大小限制会报错
func downloadVoiceBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("构造语音下载请求失败: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载语音附件失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载语音附件返回非200状态码: %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxVoiceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取语音附件内容失败: %w", err)
	}
	if len(data) > maxVoiceBytes {
		return nil, fmt.Errorf("语音附件超过大小限制(%d字节)", maxVoiceBytes)
	}
	return data, nil
}

// resolveUserInput 若 attachments 里有语音附件则下载并调ASR转写成文字；
// needFallback=true 表示语音处理失败/未配置，调用方应该直接回复兜底文案，不再往下走文字流程。
func (p Processor) resolveUserInput(
	ctx context.Context, input string, attachments []*dto.MessageAttachment,
) (resolved string, needFallback bool) {
	voice := findVoiceAttachment(attachments)
	if voice == nil {
		return input, false
	}
	if p.asrClient == nil {
		log.Printf("[Voice] 未配置腾讯云ASR，语音消息无法识别")
		return "", true
	}
	audioBytes, err := downloadVoiceBytes(ctx, voice.URL)
	if err != nil {
		log.Printf("[Voice] 下载语音附件失败: %v", err)
		return "", true
	}
	text, err := p.asrClient.Transcribe(ctx, audioBytes)
	if err != nil {
		log.Printf("[Voice] 语音识别失败: %v", err)
		return "", true
	}
	log.Printf("[Voice] 语音识别结果: %s", text)
	return text, false
}

// genVoiceFallbackMessage 语音识别失败或未配置ASR时的兜底回复文案
func genVoiceFallbackMessage(data dto.Message) *dto.MessageToCreate {
	return &dto.MessageToCreate{
		Timestamp: time.Now().UnixMilli(),
		Content:   "没听清，可以再说一次或打字告诉我吗",
		MessageReference: &dto.MessageReference{
			MessageID:             data.ID,
			IgnoreGetMessageError: true,
		},
		MsgID: data.ID,
	}
}
```

- [ ] **Step 4: 给 `Processor` 结构体加字段**

打开 `examples/receive-and-send/process.go`，找到：

```go
// Processor is a struct to process message
type Processor struct {
	api     openapi.OpenAPI
	bailian *BailianClient
	faq     []FAQEntry
	memory  *MemoryStore
}
```

改成：

```go
// Processor is a struct to process message
type Processor struct {
	api       openapi.OpenAPI
	bailian   *BailianClient
	faq       []FAQEntry
	memory    *MemoryStore
	asrClient voiceTranscriber
}
```

- [ ] **Step 5: 运行测试，确认通过**

Run: `cd examples/receive-and-send && go test ./... -run 'TestFindVoiceAttachment|TestDownloadVoiceBytes' -v`
Expected: 全部 `PASS`。

- [ ] **Step 6: 补一组 `resolveUserInput` 的单测**

在 `asr_test.go` 追加：

```go
type fakeTranscriber struct {
	text string
	err  error
}

func (f fakeTranscriber) Transcribe(_ context.Context, _ []byte) (string, error) {
	return f.text, f.err
}

func TestResolveUserInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fake-silk-bytes"))
	}))
	defer srv.Close()
	voiceAttachments := []*dto.MessageAttachment{{ContentType: "voice", URL: srv.URL}}

	t.Run("没有语音附件，原样返回文字输入", func(t *testing.T) {
		p := Processor{asrClient: fakeTranscriber{text: "不应该被调用"}}
		got, needFallback := p.resolveUserInput(context.Background(), "你好", nil)
		if needFallback || got != "你好" {
			t.Fatalf("got (%q,%v)", got, needFallback)
		}
	})

	t.Run("未配置ASR客户端", func(t *testing.T) {
		p := Processor{asrClient: nil}
		_, needFallback := p.resolveUserInput(context.Background(), "", voiceAttachments)
		if !needFallback {
			t.Fatal("期望 needFallback=true")
		}
	})

	t.Run("识别成功", func(t *testing.T) {
		p := Processor{asrClient: fakeTranscriber{text: "今天天气怎么样"}}
		got, needFallback := p.resolveUserInput(context.Background(), "", voiceAttachments)
		if needFallback || got != "今天天气怎么样" {
			t.Fatalf("got (%q,%v)", got, needFallback)
		}
	})

	t.Run("识别失败", func(t *testing.T) {
		p := Processor{asrClient: fakeTranscriber{err: errors.New("boom")}}
		_, needFallback := p.resolveUserInput(context.Background(), "", voiceAttachments)
		if !needFallback {
			t.Fatal("期望 needFallback=true")
		}
	})
}

func TestGenVoiceFallbackMessage(t *testing.T) {
	msg := genVoiceFallbackMessage(dto.Message{ID: "msg-123"})
	if msg.MsgID != "msg-123" {
		t.Fatalf("got MsgID=%q", msg.MsgID)
	}
	if msg.Content == "" {
		t.Fatal("期望兜底文案非空")
	}
}
```

（`errors` 包需要加进 `asr_test.go` 的 import 块。）

- [ ] **Step 7: 运行全部 asr 相关测试，确认通过**

Run: `cd examples/receive-and-send && go test ./... -v`
Expected: 全部 `PASS`（包含 Task 1 的 `TestExtractRecognizedText` 和本任务新增的测试）。

- [ ] **Step 8: Commit**

```bash
cd examples/receive-and-send
git add asr.go asr_test.go process.go
git commit -m "新增语音附件识别与查找逻辑，Processor接入voiceTranscriber接口"
```

---

### Task 3: 接入三个消息场景 + 配置项 + 本地验证

**Files:**
- Modify: `examples/receive-and-send/process.go:23-29`（`ProcessChannelMessage`）
- Modify: `examples/receive-and-send/process.go:84-94`（`ProcessGroupMessage`）
- Modify: `examples/receive-and-send/process.go:99-109`（`ProcessC2CMessage`）
- Modify: `examples/receive-and-send/main.go`（`Config` 结构体 + `main()` 里构造 `Processor` 那一段）
- Modify: `examples/receive-and-send/config.yaml`（本地文件，**不提交 git**，新增腾讯云ASR配置项）

**Interfaces:**
- Consumes：Task 2 的 `(p Processor) resolveUserInput(...)`、`genVoiceFallbackMessage(...)`、`voiceTranscriber`；Task 1 的 `NewTencentASRClient(...)`
- Produces：无新增导出接口，这是最终的"接线"任务

本任务不新增单测——`Processor` 依赖的 `openapi.OpenAPI` 是一个很大的组合接口（`openapi/iface.go`），项目里现有的 `ProcessC2CMessage` 等函数也从来没有写过单测，照搬那套 mock 成本远超收益。本任务靠 `go build`/`go vet` 保证编译期正确性，靠本地真实收发消息做手工验证（跟项目之前接百炼/Redis时"先 ngrok 本地验证"的流程一致）。

- [ ] **Step 1: 修改 `ProcessChannelMessage`**

在 `examples/receive-and-send/process.go` 里，把：

```go
// ProcessChannelMessage is a function to process message
func (p Processor) ProcessChannelMessage(input string, data *dto.WSATMessageData) error {
	// 频道场景暂不接入对话记忆
	msg := p.generateReplyMessage(input, dto.Message(*data), "")
	if err := p.sendChannelReply(context.Background(), data.ChannelID, msg); err != nil {
		_ = p.sendChannelReply(context.Background(), data.GroupID, genErrMessage(dto.Message(*data), err))
	}
	return nil
}
```

改成：

```go
// ProcessChannelMessage is a function to process message
func (p Processor) ProcessChannelMessage(input string, data *dto.WSATMessageData) error {
	ctx := context.Background()
	resolvedInput, needFallback := p.resolveUserInput(ctx, input, data.Attachments)
	if needFallback {
		return p.sendChannelReply(ctx, data.ChannelID, genVoiceFallbackMessage(dto.Message(*data)))
	}
	// 频道场景暂不接入对话记忆
	msg := p.generateReplyMessage(resolvedInput, dto.Message(*data), "")
	if err := p.sendChannelReply(ctx, data.ChannelID, msg); err != nil {
		_ = p.sendChannelReply(ctx, data.GroupID, genErrMessage(dto.Message(*data), err))
	}
	return nil
}
```

- [ ] **Step 2: 修改 `ProcessGroupMessage`**

把：

```go
// ProcessGroupMessage 回复群消息
func (p Processor) ProcessGroupMessage(input string, data *dto.WSGroupATMessageData) error {
	memberID := ""
	if data.Author != nil && data.Author.ID != "" {
		memberID = data.Author.ID
	}
	memKey := memoryKey("group", data.GroupID, memberID)
	msg := p.generateReplyMessage(input, dto.Message(*data), memKey)
	if err := p.sendGroupReply(context.Background(), data.GroupID, msg); err != nil {
		_ = p.sendGroupReply(context.Background(), data.GroupID, genErrMessage(dto.Message(*data), err))
	}

	return nil
}
```

改成：

```go
// ProcessGroupMessage 回复群消息
func (p Processor) ProcessGroupMessage(input string, data *dto.WSGroupATMessageData) error {
	ctx := context.Background()
	resolvedInput, needFallback := p.resolveUserInput(ctx, input, data.Attachments)
	if needFallback {
		return p.sendGroupReply(ctx, data.GroupID, genVoiceFallbackMessage(dto.Message(*data)))
	}

	memberID := ""
	if data.Author != nil && data.Author.ID != "" {
		memberID = data.Author.ID
	}
	memKey := memoryKey("group", data.GroupID, memberID)
	msg := p.generateReplyMessage(resolvedInput, dto.Message(*data), memKey)
	if err := p.sendGroupReply(ctx, data.GroupID, msg); err != nil {
		_ = p.sendGroupReply(ctx, data.GroupID, genErrMessage(dto.Message(*data), err))
	}

	return nil
}
```

- [ ] **Step 3: 修改 `ProcessC2CMessage`**

把：

```go
// ProcessC2CMessage 回复C2C消息
func (p Processor) ProcessC2CMessage(input string, data *dto.WSC2CMessageData) error {
	userID := ""
	if data.Author != nil && data.Author.ID != "" {
		userID = data.Author.ID
	}
	memKey := memoryKey("c2c", "", userID)
	msg := p.generateReplyMessage(input, dto.Message(*data), memKey)
	if err := p.sendC2CReply(context.Background(), userID, msg); err != nil {
		_ = p.sendC2CReply(context.Background(), userID, genErrMessage(dto.Message(*data), err))
	}
	return nil
}
```

改成：

```go
// ProcessC2CMessage 回复C2C消息
func (p Processor) ProcessC2CMessage(input string, data *dto.WSC2CMessageData) error {
	userID := ""
	if data.Author != nil && data.Author.ID != "" {
		userID = data.Author.ID
	}
	ctx := context.Background()
	resolvedInput, needFallback := p.resolveUserInput(ctx, input, data.Attachments)
	if needFallback {
		return p.sendC2CReply(ctx, userID, genVoiceFallbackMessage(dto.Message(*data)))
	}

	memKey := memoryKey("c2c", "", userID)
	msg := p.generateReplyMessage(resolvedInput, dto.Message(*data), memKey)
	if err := p.sendC2CReply(ctx, userID, msg); err != nil {
		_ = p.sendC2CReply(ctx, userID, genErrMessage(dto.Message(*data), err))
	}
	return nil
}
```

- [ ] **Step 4: 给 `main.go` 的 `Config` 结构体加字段**

把：

```go
// Config 配置文件结构
type Config struct {
	AppID         string `yaml:"appid"`
	Secret        string `yaml:"secret"`
	BailianAPIKey string `yaml:"bailian_api_key"`
	BailianModel  string `yaml:"bailian_model"`
	RedisAddr     string `yaml:"redis_addr"`
	RedisPassword string `yaml:"redis_password"`
	RedisDB       int    `yaml:"redis_db"`
}
```

改成：

```go
// Config 配置文件结构
type Config struct {
	AppID                    string `yaml:"appid"`
	Secret                   string `yaml:"secret"`
	BailianAPIKey            string `yaml:"bailian_api_key"`
	BailianModel             string `yaml:"bailian_model"`
	RedisAddr                string `yaml:"redis_addr"`
	RedisPassword            string `yaml:"redis_password"`
	RedisDB                  int    `yaml:"redis_db"`
	TencentSecretID          string `yaml:"tencent_secret_id"`
	TencentSecretKey         string `yaml:"tencent_secret_key"`
	TencentASRRegion         string `yaml:"tencent_asr_region"`
	TencentASREngServiceType string `yaml:"tencent_asr_eng_service_type"`
}
```

- [ ] **Step 5: 在 `main()` 里构造 ASR 客户端并传给 `Processor`**

把 `main.go` 里原来的：

```go
	processor = Processor{api: api, bailian: bailian, faq: faqEntries, memory: memory}
```

改成（插在这一行之前）：

```go
	// 留空 tencent_secret_id 则不启用语音识别功能，语音消息会收到固定兜底文案
	var asrClient voiceTranscriber
	if config.TencentSecretID != "" {
		client, err := NewTencentASRClient(
			config.TencentSecretID, config.TencentSecretKey,
			config.TencentASRRegion, config.TencentASREngServiceType,
		)
		if err != nil {
			log.Printf("[Voice] 创建腾讯云ASR客户端失败，语音识别功能不可用: %v", err)
		} else {
			asrClient = client
			log.Printf("[Voice] 腾讯云ASR客户端已创建，语音识别功能已启用")
		}
	}
	processor = Processor{api: api, bailian: bailian, faq: faqEntries, memory: memory, asrClient: asrClient}
```

- [ ] **Step 6: 编译与静态检查**

Run:
```bash
cd examples/receive-and-send
go build .
go vet .
```
Expected: 两条命令都无报错退出。

- [ ] **Step 7: 跑一遍完整测试套件**

Run: `cd examples/receive-and-send && go test ./... -v`
Expected: 全部 `PASS`。

- [ ] **Step 8: 补本地 `config.yaml`（不提交 git）**

在本地 `examples/receive-and-send/config.yaml` 文件末尾追加（去腾讯云控制台开"语音识别"服务权限后填真实密钥，没有密钥时留空即可验证"未配置自动降级"分支）：

```yaml

# 腾讯云语音识别(一句话识别)配置，用于识别用户发来的语音消息
# 留空 tencent_secret_id 则不启用语音识别功能
tencent_secret_id:
tencent_secret_key:
tencent_asr_region: ap-guangzhou
tencent_asr_eng_service_type: 16k_zh
```

- [ ] **Step 9: 本地手工验证**

跟之前接入百炼大模型/Redis对话记忆时同样的流程：起本地 ngrok 隧道把 QQ 开放平台的回调地址指向本机，私聊机器人发一条语音消息，观察日志：
- 看到 `[Voice] 腾讯云ASR客户端已创建，语音识别功能已启用`（启动时）
- 发语音后看到 `[Voice] 语音识别结果: <识别出的文字>`
- 确认机器人用文字回复了识别出的内容（FAQ/百炼正常生效）
- 把 `config.yaml` 的 `tencent_secret_id` 清空重启后再发一次语音，确认收到"没听清，可以再说一次或打字告诉我吗"这条兜底回复，且日志里能看到 `[Voice] 未配置腾讯云ASR，语音消息无法识别`

- [ ] **Step 10: Commit**

```bash
cd examples/receive-and-send
git add process.go main.go
git commit -m "接入语音消息识别：私聊/群聊@/频道收到语音时转文字再走现有FAQ/百炼流程"
```

---

## 完成后

Plan A（语音输入/ASR）到此完整可用：机器人能听懂三个场景的语音消息并用文字回复。Plan B（TTS 语音回复，仅私聊+群聊@，需要引入 CGO silk 编码 + 腾讯云 COS）等这部分上线验证过后再单独走一轮设计确认 + `writing-plans`。
