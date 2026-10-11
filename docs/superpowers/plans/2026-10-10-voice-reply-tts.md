# 语音回复(TTS) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 当用户用语音提问时，机器人用语音回复（私聊/群聊@），把大模型/FAQ产出的文字转成语音消息发回去；任何一步失败都退化成发文字，不影响现有功能。

**Architecture:** 新增 `tts.go`（腾讯云语音合成）、`silk.go`（纯 Go 库把 PCM 编码成 silk v3）、`cos.go`（上传临时文件到腾讯云 COS 并生成预签名 URL）三个独立模块，外加一个编排函数 `sendVoiceReply` 串起"文本→语音→上传→QQ富媒体两步发送"整条链。`asr.go` 的 `resolveUserInput` 改造出一个 `fromVoice` 信号，`process.go` 里私聊/群聊@ 两个处理函数在 `fromVoice==true` 时尝试走语音回复，失败则照常发文字。

**Tech Stack:** Go 1.21。`github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/tts/v20190823`（TTS，跟 Plan A 的 ASR 同一个 SDK 仓库、同样的调用范式）；`github.com/youthlin/silk`（纯 Go silk v3 编解码，从 C 版本移植）；`github.com/tencentyun/cos-go-sdk-v5`（COS 官方 SDK）。

## Global Constraints

- 腾讯云"基础语音合成"单次最多 **150 个汉字**（全角标点算一个字）。超过要截断——截断后的文本加上提示语本身也必须落在 150 字预算内（提示语占用字数要从截断长度里扣除，不是额外加在 150 字之外）。
- TTS 输出用 `Codec=pcm`、`SampleRate=24000`；silk 编码对应用 `silk.SampleRate(24000)`，两边采样率必须一致。
- 本计划**不引入 CGO**，继续 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build`（`examples/receive-and-send/Makefile` 不改）。
- COS 用**预签名 URL**（~10分钟时效）访问上传的语音文件，不把 bucket 设成公开读。
- 只接入**私聊 + 群聊@**两个场景；频道场景不接语音回复（QQ API 不支持，`ProcessChannelMessage` 本任务不改动）。
- 新代码风格跟现有文件一致：中文日志（`[Voice]` 前缀）、`fmt.Errorf("...: %w", err)` 包装错误。
- `examples/` 是独立 go module，依赖变更要在 `examples/` 目录下执行 `go get`/`go mod tidy`。
- **降级原则**：`sendVoiceReply` 链路里任何一步失败，都返回 error，调用方统一降级发送已经生成好的文字回复（不重新调用大模型），绝不能让用户什么都收不到。
- 设计依据：`docs/superpowers/specs/2026-10-09-voice-message-design.md`（Plan B 部分，2026-10-09 修订）。

---

### Task 1: 腾讯云 TTS 客户端

**Files:**
- Create: `examples/receive-and-send/tts.go`
- Create: `examples/receive-and-send/tts_test.go`
- Modify: `examples/receive-and-send/go.mod`, `examples/receive-and-send/go.sum`（`go get`/`go mod tidy` 自动更新）

**Interfaces:**
- Produces：
  - `const maxTTSChars = 150`
  - `func truncateForTTS(text string) string`
  - `type TencentTTSClient struct{...}`
  - `func NewTencentTTSClient(secretID, secretKey, region string, voiceType int64) (*TencentTTSClient, error)`
  - `func (c *TencentTTSClient) Synthesize(ctx context.Context, text string) ([]byte, error)`（返回原始 PCM 字节）
  - `func extractAudio(resp *tts.TextToVoiceResponse) ([]byte, error)`（包内私有）

- [ ] **Step 1: 添加腾讯云 TTS SDK 依赖**

Run:
```bash
cd examples
go get github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/tts/v20190823@latest
go mod tidy
```
Expected: `go.mod` 新增一条 `.../tencentcloud/tts/v20190823` 的 direct require，`go.sum` 相应更新。

- [ ] **Step 2: 写失败的单测（`truncateForTTS`）**

创建 `examples/receive-and-send/tts_test.go`：

```go
package main

import "testing"

func TestTruncateForTTS(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "短文本不截断", input: "你好", want: "你好"},
		{
			name:  "超长文本截断并在150字预算内附加提示语",
			input: func() string {
				runes := make([]rune, 200)
				for i := range runes {
					runes[i] = '字'
				}
				return string(runes)
			}(),
			want: func() string {
				runes := make([]rune, 135)
				for i := range runes {
					runes[i] = '字'
				}
				return string(runes) + "（回复较长，完整内容请看文字）"
			}(),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncateForTTS(c.input)
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
			if n := len([]rune(got)); n > maxTTSChars {
				t.Fatalf("截断结果仍超过%d字: 实际%d字", maxTTSChars, n)
			}
		})
	}
}
```

- [ ] **Step 3: 运行测试，确认失败**

Run: `cd examples/receive-and-send && go test ./... -run TestTruncateForTTS -v`
Expected: 编译失败，报 `undefined: truncateForTTS` / `undefined: maxTTSChars`。

- [ ] **Step 4: 实现 `tts.go`**

创建 `examples/receive-and-send/tts.go`：

```go
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	tts "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/tts/v20190823"
)

// maxTTSChars 腾讯云"基础语音合成"单次最多支持150个汉字，提示语本身也要算进这个预算
const maxTTSChars = 150

// truncatedTip 超长文本截断后附加的提示语
const truncatedTip = "（回复较长，完整内容请看文字）"

// truncateForTTS 把文本截断到 maxTTSChars 字以内；超长时截断正文并附加提示语，
// 提示语本身占用的字数要从截断长度里扣除，保证总长度不超预算。
func truncateForTTS(text string) string {
	runes := []rune(text)
	if len(runes) <= maxTTSChars {
		return text
	}
	tipRunes := []rune(truncatedTip)
	keep := maxTTSChars - len(tipRunes)
	if keep < 0 {
		keep = 0
	}
	return string(runes[:keep]) + truncatedTip
}

// TencentTTSClient 封装对腾讯云"基础语音合成"接口的调用
type TencentTTSClient struct {
	client    *tts.Client
	voiceType int64
}

// NewTencentTTSClient 创建一个腾讯云TTS客户端
func NewTencentTTSClient(secretID, secretKey, region string, voiceType int64) (*TencentTTSClient, error) {
	credential := common.NewCredential(secretID, secretKey)
	cpf := profile.NewClientProfile()
	client, err := tts.NewClient(credential, region, cpf)
	if err != nil {
		return nil, fmt.Errorf("创建腾讯云TTS客户端失败: %w", err)
	}
	return &TencentTTSClient{client: client, voiceType: voiceType}, nil
}

// Synthesize 调腾讯云基础语音合成，输入文本（会先截断到150字以内），
// 输出 PCM 裸音频字节（16位有符号小端，24000采样率，单声道），用于喂给 silk 编码器。
func (c *TencentTTSClient) Synthesize(ctx context.Context, text string) ([]byte, error) {
	truncated := truncateForTTS(text)

	request := tts.NewTextToVoiceRequest()
	request.Text = common.StringPtr(truncated)
	request.SessionId = common.StringPtr(fmt.Sprintf("%d", time.Now().UnixNano()))
	request.VoiceType = common.Int64Ptr(c.voiceType)
	request.SampleRate = common.Uint64Ptr(24000)
	request.Codec = common.StringPtr("pcm")

	response, err := c.client.TextToVoiceWithContext(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("调用腾讯云TTS接口失败: %w", err)
	}
	return extractAudio(response)
}

// extractAudio 从腾讯云TTS响应里提取base64解码后的音频字节
func extractAudio(resp *tts.TextToVoiceResponse) ([]byte, error) {
	if resp == nil || resp.Response == nil || resp.Response.Audio == nil || *resp.Response.Audio == "" {
		return nil, errors.New("腾讯云TTS未返回音频数据")
	}
	data, err := base64.StdEncoding.DecodeString(*resp.Response.Audio)
	if err != nil {
		return nil, fmt.Errorf("解码腾讯云TTS音频数据失败: %w", err)
	}
	return data, nil
}
```

- [ ] **Step 5: 运行测试，确认通过**

Run: `cd examples/receive-and-send && go test ./... -run TestTruncateForTTS -v`
Expected: `PASS`。

- [ ] **Step 6: 补一组 `extractAudio` 的单测**

在 `tts_test.go` 追加（`import` 块加上 `encoding/base64`、`tts "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/tts/v20190823"`）：

```go
func strPtrTTS(s string) *string { return &s }

func TestExtractAudio(t *testing.T) {
	validAudio := base64.StdEncoding.EncodeToString([]byte("fake-pcm-bytes"))

	cases := []struct {
		name    string
		resp    *tts.TextToVoiceResponse
		want    []byte
		wantErr bool
	}{
		{name: "nil响应", resp: nil, wantErr: true},
		{name: "nil Response字段", resp: &tts.TextToVoiceResponse{}, wantErr: true},
		{
			name:    "nil Audio",
			resp:    &tts.TextToVoiceResponse{Response: &tts.TextToVoiceResponseParams{}},
			wantErr: true,
		},
		{
			name:    "空字符串Audio",
			resp:    &tts.TextToVoiceResponse{Response: &tts.TextToVoiceResponseParams{Audio: strPtrTTS("")}},
			wantErr: true,
		},
		{
			name:    "非法base64",
			resp:    &tts.TextToVoiceResponse{Response: &tts.TextToVoiceResponseParams{Audio: strPtrTTS("不是base64")}},
			wantErr: true,
		},
		{
			name: "正常音频数据",
			resp: &tts.TextToVoiceResponse{Response: &tts.TextToVoiceResponseParams{Audio: strPtrTTS(validAudio)}},
			want: []byte("fake-pcm-bytes"),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := extractAudio(c.resp)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际返回: %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不期望报错: %v", err)
			}
			if string(got) != string(c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
```

- [ ] **Step 7: 运行测试，确认通过**

Run: `cd examples/receive-and-send && go test ./... -v`
Expected: 全部 `PASS`（包括 Plan A 遗留的 ASR 测试，不应受影响）。

- [ ] **Step 8: Commit**

```bash
cd examples/receive-and-send
git add tts.go tts_test.go go.mod go.sum
git commit -m "新增腾讯云TTS客户端，封装基础语音合成接口调用与150字截断逻辑"
```

---

### Task 2: silk 编码封装（纯 Go，无 CGO）

**Files:**
- Create: `examples/receive-and-send/silk.go`
- Create: `examples/receive-and-send/silk_test.go`
- Modify: `examples/receive-and-send/go.mod`, `examples/receive-and-send/go.sum`

**Interfaces:**
- Produces：`func EncodeToSilk(pcm []byte) ([]byte, error)`

- [ ] **Step 1: 添加 silk 编解码依赖**

Run:
```bash
cd examples
go get github.com/youthlin/silk@latest
go mod tidy
```
Expected: `go.mod` 新增 `github.com/youthlin/silk` 的 direct require。

- [ ] **Step 2: 写失败的单测（编码-解码往返验证）**

创建 `examples/receive-and-send/silk_test.go`：

```go
package main

import (
	"bytes"
	"testing"

	"github.com/youthlin/silk"
)

// generateSilencePCM 生成指定时长的静音PCM（16位有符号小端，24000采样率，单声道）
func generateSilencePCM(durationSeconds float64) []byte {
	sampleCount := int(24000 * durationSeconds)
	return make([]byte, sampleCount*2) // 每个采样点2字节，静音全是0也是合法PCM
}

func TestEncodeToSilk(t *testing.T) {
	pcm := generateSilencePCM(0.5) // 0.5秒静音

	silkBytes, err := EncodeToSilk(pcm)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if len(silkBytes) == 0 {
		t.Fatal("编码结果为空")
	}

	// 往返验证：用同一个库的 Decode 把编码结果解回 PCM，确认产出的是能被正常解析的合法 silk 数据，
	// 而不是只看 Encode 没报错——这是我们能在不接真实QQ账号的情况下验证编码质量的最佳手段。
	decoded, err := silk.Decode(bytes.NewReader(silkBytes))
	if err != nil {
		t.Fatalf("解码编码结果失败（说明Encode产出的silk数据有问题）: %v", err)
	}
	if len(decoded) == 0 {
		t.Fatal("解码结果为空")
	}
}

func TestEncodeToSilkEmptyInput(t *testing.T) {
	if _, err := EncodeToSilk([]byte{}); err == nil {
		t.Fatal("期望空PCM输入报错，实际没有报错")
	}
}
```

- [ ] **Step 3: 运行测试，确认失败**

Run: `cd examples/receive-and-send && go test ./... -run TestEncodeToSilk -v`
Expected: 编译失败，报 `undefined: EncodeToSilk`。

- [ ] **Step 4: 实现 `silk.go`**

创建 `examples/receive-and-send/silk.go`：

```go
package main

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/youthlin/silk"
)

// EncodeToSilk 把PCM裸音频（16位有符号小端，24000采样率，单声道）编码成silk v3格式，
// 用的是纯Go库(github.com/youthlin/silk)，不涉及CGO。这个库默认开启Stx选项，
// 按作者说明是专门兼容QQ/微信这类场景的。
func EncodeToSilk(pcm []byte) ([]byte, error) {
	if len(pcm) == 0 {
		return nil, errors.New("PCM输入为空")
	}
	silkBytes, err := silk.Encode(bytes.NewReader(pcm), silk.SampleRate(24000))
	if err != nil {
		return nil, fmt.Errorf("silk编码失败: %w", err)
	}
	return silkBytes, nil
}
```

- [ ] **Step 5: 运行测试，确认通过**

Run: `cd examples/receive-and-send && go test ./... -v`
Expected: 全部 `PASS`。

- [ ] **Step 6: Commit**

```bash
cd examples/receive-and-send
git add silk.go silk_test.go go.mod go.sum
git commit -m "新增silk编码封装，用纯Go库 youthlin/silk 实现PCM转silk v3，不需要CGO"
```

---

### Task 3: COS 上传封装

**Files:**
- Create: `examples/receive-and-send/cos.go`
- Create: `examples/receive-and-send/cos_test.go`
- Modify: `examples/receive-and-send/go.mod`, `examples/receive-and-send/go.sum`

**Interfaces:**
- Produces：
  - `func buildBucketURL(bucket, region string) (*url.URL, error)`（包内私有，纯函数）
  - `type COSUploader struct{...}`
  - `func NewCOSUploader(secretID, secretKey, bucket, region string) (*COSUploader, error)`
  - `func (u *COSUploader) UploadAndSign(ctx context.Context, objectKey string, data []byte) (string, error)`

- [ ] **Step 1: 添加 COS SDK 依赖**

Run:
```bash
cd examples
go get github.com/tencentyun/cos-go-sdk-v5@latest
go mod tidy
```
Expected: `go.mod` 新增 `github.com/tencentyun/cos-go-sdk-v5` 的 direct require。

- [ ] **Step 2: 写失败的单测（`buildBucketURL`）**

创建 `examples/receive-and-send/cos_test.go`：

```go
package main

import "testing"

func TestBuildBucketURL(t *testing.T) {
	u, err := buildBucketURL("my-bucket-1250000000", "ap-guangzhou")
	if err != nil {
		t.Fatalf("不期望报错: %v", err)
	}
	want := "https://my-bucket-1250000000.cos.ap-guangzhou.myqcloud.com"
	if u.String() != want {
		t.Fatalf("got %q, want %q", u.String(), want)
	}
}

func TestBuildBucketURLEmptyBucket(t *testing.T) {
	if _, err := buildBucketURL("", "ap-guangzhou"); err == nil {
		t.Fatal("期望bucket为空时报错，实际没有报错")
	}
}
```

- [ ] **Step 3: 运行测试，确认失败**

Run: `cd examples/receive-and-send && go test ./... -run TestBuildBucketURL -v`
Expected: 编译失败，报 `undefined: buildBucketURL`。

- [ ] **Step 4: 实现 `cos.go`**

创建 `examples/receive-and-send/cos.go`：

```go
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/tencentyun/cos-go-sdk-v5"
)

// presignedURLExpiry 预签名URL的有效期，QQ服务器需要在这个时间窗口内把文件拉走
const presignedURLExpiry = 10 * time.Minute

// buildBucketURL 拼出 COS bucket 的访问域名
func buildBucketURL(bucket, region string) (*url.URL, error) {
	if bucket == "" || region == "" {
		return nil, errors.New("COS bucket/region 不能为空")
	}
	return url.Parse(fmt.Sprintf("https://%s.cos.%s.myqcloud.com", bucket, region))
}

// COSUploader 封装上传文件到腾讯云COS并生成预签名URL
type COSUploader struct {
	client    *cos.Client
	secretID  string
	secretKey string
}

// NewCOSUploader 创建一个COS上传器
func NewCOSUploader(secretID, secretKey, bucket, region string) (*COSUploader, error) {
	bucketURL, err := buildBucketURL(bucket, region)
	if err != nil {
		return nil, fmt.Errorf("构造COS bucket地址失败: %w", err)
	}
	client := cos.NewClient(&cos.BaseURL{BucketURL: bucketURL}, &http.Client{
		Timeout: 30 * time.Second,
		Transport: &cos.AuthorizationTransport{
			SecretID:  secretID,
			SecretKey: secretKey,
		},
	})
	return &COSUploader{client: client, secretID: secretID, secretKey: secretKey}, nil
}

// UploadAndSign 把data上传到COS的objectKey路径下，返回一个~10分钟时效的预签名URL供QQ服务器拉取。
// bucket保持私有读写，不依赖公开读权限。
func (u *COSUploader) UploadAndSign(ctx context.Context, objectKey string, data []byte) (string, error) {
	if _, err := u.client.Object.Put(ctx, objectKey, bytes.NewReader(data), nil); err != nil {
		return "", fmt.Errorf("上传COS失败: %w", err)
	}
	signedURL, err := u.client.Object.GetPresignedURL(
		ctx, http.MethodGet, objectKey, u.secretID, u.secretKey, presignedURLExpiry, nil,
	)
	if err != nil {
		return "", fmt.Errorf("生成COS预签名URL失败: %w", err)
	}
	return signedURL.String(), nil
}
```

- [ ] **Step 5: 运行测试，确认通过**

Run: `cd examples/receive-and-send && go test ./... -v`
Expected: 全部 `PASS`。

- [ ] **Step 6: Commit**

```bash
cd examples/receive-and-send
git add cos.go cos_test.go go.mod go.sum
git commit -m "新增COS上传封装，上传后生成预签名URL供QQ回调拉取"
```

---

### Task 4: fromVoice 信号 + 编排 + 接入私聊/群聊@ + 配置

**Files:**
- Modify: `examples/receive-and-send/asr.go`（`resolveUserInput` 签名加第三个返回值）
- Modify: `examples/receive-and-send/asr_test.go`（`TestResolveUserInput` 各子测试加一个返回值断言）
- Modify: `examples/receive-and-send/process.go`（`Processor` 结构体加两个字段；`ProcessC2CMessage`/`ProcessGroupMessage` 接入语音回复；`ProcessChannelMessage` 不动）
- Create: `examples/receive-and-send/voice_reply.go`
- Create: `examples/receive-and-send/voice_reply_test.go`
- Modify: `examples/receive-and-send/main.go`（`Config` 结构体 + `main()` 构造 ttsClient/cosUploader）
- Modify: `examples/receive-and-send/config.yaml`（本地文件，**不提交 git**）

**Interfaces:**
- Consumes：Task 1 的 `(*TencentTTSClient).Synthesize(ctx, text string) ([]byte, error)`；Task 2 的 `EncodeToSilk(pcm []byte) ([]byte, error)`；Task 3 的 `(*COSUploader).UploadAndSign(ctx, objectKey string, data []byte) (string, error)`
- Produces：
  - `resolveUserInput` 新签名：`func (p Processor) resolveUserInput(ctx context.Context, input string, attachments []*dto.MessageAttachment) (resolved string, needFallback bool, fromVoice bool)`
  - `type voiceReplySynthesizer interface { Synthesize(ctx context.Context, text string) ([]byte, error) }`
  - `type voiceReplyUploader interface { UploadAndSign(ctx context.Context, objectKey string, data []byte) (string, error) }`
  - `func synthesizeVoiceURL(ctx context.Context, tts voiceReplySynthesizer, cos voiceReplyUploader, text string) (string, error)`
  - `func (p Processor) sendVoiceReply(ctx context.Context, scene, targetID, text, refMsgID string) error`
  - `Processor` 结构体新增字段：`ttsClient voiceReplySynthesizer`、`cosUploader voiceReplyUploader`

本任务不新增 `openapi.OpenAPI` 相关的 mock——理由跟 Plan A 的 Task 3 一样：这个接口太大，照搬一遍 mock 成本远超收益。`sendVoiceReply` 里"调 `p.api.PostC2CMessage`/`PostGroupMessage`"这部分靠本任务最后的本地真实语音消息手工验证来确认，不是单测覆盖范围；但 `synthesizeVoiceURL`（TTS→编码→上传，不碰 `p.api`）是可以且应该用假实现单测的。

- [ ] **Step 1: 给 `resolveUserInput` 加 `fromVoice` 返回值**

打开 `examples/receive-and-send/asr.go`，找到：

```go
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

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

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
```

改成：

```go
// resolveUserInput 若 attachments 里有语音附件则下载并调ASR转写成文字；
// needFallback=true 表示语音处理失败/未配置，调用方应该直接回复兜底文案，不再往下走文字流程；
// fromVoice=true 表示这轮确实是语音输入且识别成功，调用方可以据此决定要不要尝试语音回复。
func (p Processor) resolveUserInput(
	ctx context.Context, input string, attachments []*dto.MessageAttachment,
) (resolved string, needFallback bool, fromVoice bool) {
	voice := findVoiceAttachment(attachments)
	if voice == nil {
		return input, false, false
	}
	if p.asrClient == nil {
		log.Printf("[Voice] 未配置腾讯云ASR，语音消息无法识别")
		return "", true, false
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	audioBytes, err := downloadVoiceBytes(ctx, voice.URL)
	if err != nil {
		log.Printf("[Voice] 下载语音附件失败: %v", err)
		return "", true, false
	}
	text, err := p.asrClient.Transcribe(ctx, audioBytes)
	if err != nil {
		log.Printf("[Voice] 语音识别失败: %v", err)
		return "", true, false
	}
	log.Printf("[Voice] 语音识别结果: %s", text)
	return text, false, true
}
```

- [ ] **Step 2: 更新 `TestResolveUserInput` 的断言**

打开 `examples/receive-and-send/asr_test.go`，找到 `TestResolveUserInput` 函数，把四个子测试改成：

```go
func TestResolveUserInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fake-silk-bytes"))
	}))
	defer srv.Close()
	voiceAttachments := []*dto.MessageAttachment{{ContentType: "voice", URL: srv.URL}}

	t.Run("没有语音附件，原样返回文字输入", func(t *testing.T) {
		p := Processor{asrClient: fakeTranscriber{text: "不应该被调用"}}
		got, needFallback, fromVoice := p.resolveUserInput(context.Background(), "你好", nil)
		if needFallback || fromVoice || got != "你好" {
			t.Fatalf("got (%q,%v,%v)", got, needFallback, fromVoice)
		}
	})

	t.Run("未配置ASR客户端", func(t *testing.T) {
		p := Processor{asrClient: nil}
		_, needFallback, fromVoice := p.resolveUserInput(context.Background(), "", voiceAttachments)
		if !needFallback || fromVoice {
			t.Fatalf("got (needFallback=%v,fromVoice=%v)", needFallback, fromVoice)
		}
	})

	t.Run("识别成功", func(t *testing.T) {
		p := Processor{asrClient: fakeTranscriber{text: "今天天气怎么样"}}
		got, needFallback, fromVoice := p.resolveUserInput(context.Background(), "", voiceAttachments)
		if needFallback || !fromVoice || got != "今天天气怎么样" {
			t.Fatalf("got (%q,%v,%v)", got, needFallback, fromVoice)
		}
	})

	t.Run("识别失败", func(t *testing.T) {
		p := Processor{asrClient: fakeTranscriber{err: errors.New("boom")}}
		_, needFallback, fromVoice := p.resolveUserInput(context.Background(), "", voiceAttachments)
		if !needFallback || fromVoice {
			t.Fatalf("got (needFallback=%v,fromVoice=%v)", needFallback, fromVoice)
		}
	})
}
```

- [ ] **Step 3: 运行测试，确认失败**

Run: `cd examples/receive-and-send && go test ./... -run TestResolveUserInput -v`
Expected: 编译失败（函数签名对不上，调用处只接收两个返回值）。

- [ ] **Step 4: 运行测试，确认通过**

Run: `cd examples/receive-and-send && go test ./... -v`
Expected: 这一步会先因为 `process.go` 里原来调用 `p.resolveUserInput(...)` 只接两个返回值而编译失败——这是预期的，Step 6 会修 `process.go`。先确认 `asr_test.go` 本身逻辑没问题即可，继续往下做。

- [ ] **Step 5: 写失败的单测（`synthesizeVoiceURL`）**

创建 `examples/receive-and-send/voice_reply_test.go`：

```go
package main

import (
	"context"
	"errors"
	"testing"
)

type fakeTTSSynthesizer struct {
	pcm []byte
	err error
}

func (f fakeTTSSynthesizer) Synthesize(_ context.Context, _ string) ([]byte, error) {
	return f.pcm, f.err
}

type fakeCOSUploader struct {
	url string
	err error
}

func (f fakeCOSUploader) UploadAndSign(_ context.Context, _ string, _ []byte) (string, error) {
	return f.url, f.err
}

func TestSynthesizeVoiceURL(t *testing.T) {
	t.Run("TTS失败", func(t *testing.T) {
		_, err := synthesizeVoiceURL(
			context.Background(),
			fakeTTSSynthesizer{err: errors.New("tts boom")},
			fakeCOSUploader{url: "https://example.com/x.silk"},
			"你好",
		)
		if err == nil {
			t.Fatal("期望报错，实际没有报错")
		}
	})

	t.Run("编码失败(空PCM)", func(t *testing.T) {
		_, err := synthesizeVoiceURL(
			context.Background(),
			fakeTTSSynthesizer{pcm: []byte{}},
			fakeCOSUploader{url: "https://example.com/x.silk"},
			"你好",
		)
		if err == nil {
			t.Fatal("期望报错，实际没有报错")
		}
	})

	t.Run("COS上传失败", func(t *testing.T) {
		_, err := synthesizeVoiceURL(
			context.Background(),
			fakeTTSSynthesizer{pcm: generateSilencePCM(0.1)},
			fakeCOSUploader{err: errors.New("cos boom")},
			"你好",
		)
		if err == nil {
			t.Fatal("期望报错，实际没有报错")
		}
	})

	t.Run("成功", func(t *testing.T) {
		url, err := synthesizeVoiceURL(
			context.Background(),
			fakeTTSSynthesizer{pcm: generateSilencePCM(0.1)},
			fakeCOSUploader{url: "https://example.com/x.silk"},
			"你好",
		)
		if err != nil {
			t.Fatalf("不期望报错: %v", err)
		}
		if url != "https://example.com/x.silk" {
			t.Fatalf("got %q", url)
		}
	})
}
```

（`generateSilencePCM` 是 Task 2 在 `silk_test.go` 里定义的辅助函数，同包内直接可用，不用重复定义。）

- [ ] **Step 6: 实现 `voice_reply.go`，并修 `process.go`/`asr_test.go` 的编译**

创建 `examples/receive-and-send/voice_reply.go`：

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tencent-connect/botgo/dto"
)

// voiceReplySynthesizer 抽象TTS客户端能力，方便单测时用假实现替换
type voiceReplySynthesizer interface {
	Synthesize(ctx context.Context, text string) ([]byte, error)
}

// voiceReplyUploader 抽象COS上传能力，方便单测时用假实现替换
type voiceReplyUploader interface {
	UploadAndSign(ctx context.Context, objectKey string, data []byte) (string, error)
}

// synthesizeVoiceURL 把文本转成语音并上传，返回一个供QQ服务器拉取的URL。
// 不涉及openapi.OpenAPI调用，可以用假实现单测。
func synthesizeVoiceURL(
	ctx context.Context, tts voiceReplySynthesizer, cos voiceReplyUploader, text string,
) (string, error) {
	pcm, err := tts.Synthesize(ctx, text)
	if err != nil {
		return "", fmt.Errorf("TTS调用失败: %w", err)
	}
	silkBytes, err := EncodeToSilk(pcm)
	if err != nil {
		return "", fmt.Errorf("silk编码失败: %w", err)
	}
	url, err := cos.UploadAndSign(ctx, voiceObjectKey(), silkBytes)
	if err != nil {
		return "", fmt.Errorf("COS上传失败: %w", err)
	}
	return url, nil
}

// voiceObjectKey 生成COS对象key，统一放在voice-reply/前缀下方便按前缀管理/设生命周期规则，
// 用纳秒级时间戳区分不同消息。
func voiceObjectKey() string {
	return fmt.Sprintf("voice-reply/%d.silk", time.Now().UnixNano())
}

// sendVoiceReply 把text转成语音回复发给用户。scene是"c2c"或"group"，targetID是对应的userID/groupID，
// refMsgID是要回复的原始消息ID。任何一步失败都返回error，调用方应该降级发文字。
func (p Processor) sendVoiceReply(ctx context.Context, scene, targetID, text, refMsgID string) error {
	if p.ttsClient == nil || p.cosUploader == nil {
		return errors.New("TTS/COS未配置")
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// 注意：截断逻辑在 TencentTTSClient.Synthesize 内部（Task 1）已经处理，这里不用重复截断
	url, err := synthesizeVoiceURL(ctx, p.ttsClient, p.cosUploader, text)
	if err != nil {
		return err
	}

	richMedia := &dto.RichMediaMessage{FileType: 3, URL: url}
	var uploadResp *dto.Message
	switch scene {
	case "c2c":
		uploadResp, err = p.api.PostC2CMessage(ctx, targetID, richMedia)
	case "group":
		uploadResp, err = p.api.PostGroupMessage(ctx, targetID, richMedia)
	default:
		return fmt.Errorf("不支持的语音回复场景: %s", scene)
	}
	if err != nil {
		return fmt.Errorf("富媒体上传失败: %w", err)
	}
	if uploadResp == nil || len(uploadResp.FileInfo) == 0 {
		return errors.New("富媒体上传未返回file_info")
	}

	finalMsg := &dto.MessageToCreate{
		Timestamp: time.Now().UnixMilli(),
		MsgType:   dto.RichMediaMsg,
		Media:     &dto.MediaInfo{FileInfo: uploadResp.FileInfo},
		MsgID:     refMsgID,
		MessageReference: &dto.MessageReference{
			MessageID:             refMsgID,
			IgnoreGetMessageError: true,
		},
	}
	switch scene {
	case "c2c":
		err = p.sendC2CReply(ctx, targetID, finalMsg)
	case "group":
		err = p.sendGroupReply(ctx, targetID, finalMsg)
	}
	if err != nil {
		return fmt.Errorf("语音消息发送失败: %w", err)
	}
	return nil
}
```

打开 `examples/receive-and-send/process.go`：

把 `Processor` 结构体：

```go
type Processor struct {
	api       openapi.OpenAPI
	bailian   *BailianClient
	faq       []FAQEntry
	memory    *MemoryStore
	asrClient voiceTranscriber
}
```

改成：

```go
type Processor struct {
	api         openapi.OpenAPI
	bailian     *BailianClient
	faq         []FAQEntry
	memory      *MemoryStore
	asrClient   voiceTranscriber
	ttsClient   voiceReplySynthesizer
	cosUploader voiceReplyUploader
}
```

把 `ProcessGroupMessage`：

```go
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

改成：

```go
func (p Processor) ProcessGroupMessage(input string, data *dto.WSGroupATMessageData) error {
	ctx := context.Background()
	resolvedInput, needFallback, fromVoice := p.resolveUserInput(ctx, input, data.Attachments)
	if needFallback {
		return p.sendGroupReply(ctx, data.GroupID, genVoiceFallbackMessage(dto.Message(*data)))
	}

	memberID := ""
	if data.Author != nil && data.Author.ID != "" {
		memberID = data.Author.ID
	}
	memKey := memoryKey("group", data.GroupID, memberID)
	msg := p.generateReplyMessage(resolvedInput, dto.Message(*data), memKey)

	if fromVoice {
		if err := p.sendVoiceReply(ctx, "group", data.GroupID, msg.Content, data.ID); err == nil {
			return nil
		} else {
			log.Printf("[Voice] 语音回复失败，降级为文字: %v", err)
		}
	}

	if err := p.sendGroupReply(ctx, data.GroupID, msg); err != nil {
		_ = p.sendGroupReply(ctx, data.GroupID, genErrMessage(dto.Message(*data), err))
	}

	return nil
}
```

把 `ProcessC2CMessage`：

```go
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

改成：

```go
func (p Processor) ProcessC2CMessage(input string, data *dto.WSC2CMessageData) error {
	userID := ""
	if data.Author != nil && data.Author.ID != "" {
		userID = data.Author.ID
	}
	ctx := context.Background()
	resolvedInput, needFallback, fromVoice := p.resolveUserInput(ctx, input, data.Attachments)
	if needFallback {
		return p.sendC2CReply(ctx, userID, genVoiceFallbackMessage(dto.Message(*data)))
	}

	memKey := memoryKey("c2c", "", userID)
	msg := p.generateReplyMessage(resolvedInput, dto.Message(*data), memKey)

	if fromVoice {
		if err := p.sendVoiceReply(ctx, "c2c", userID, msg.Content, data.ID); err == nil {
			return nil
		} else {
			log.Printf("[Voice] 语音回复失败，降级为文字: %v", err)
		}
	}

	if err := p.sendC2CReply(ctx, userID, msg); err != nil {
		_ = p.sendC2CReply(ctx, userID, genErrMessage(dto.Message(*data), err))
	}
	return nil
}
```

**`ProcessChannelMessage` 不改**，只需要把它里面调用 `p.resolveUserInput(...)` 那一行接收返回值的地方从两个改成三个（忽略第三个），因为函数签名已经变了：

```go
func (p Processor) ProcessChannelMessage(input string, data *dto.WSATMessageData) error {
	ctx := context.Background()
	resolvedInput, needFallback, _ := p.resolveUserInput(ctx, input, data.Attachments)
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

`process.go` 顶部需要确认 `log` 包已经 import（`ProcessGroupMessage`/`ProcessC2CMessage` 新增了 `log.Printf` 调用）——如果还没有，加上 `"log"`。

- [ ] **Step 7: 运行全部测试，确认通过**

Run: `cd examples/receive-and-send && go test ./... -v`
Expected: 全部 `PASS`（Task1/2/3 的测试 + 本任务新增的 `TestSynthesizeVoiceURL` + 更新过的 `TestResolveUserInput`）。

- [ ] **Step 8: 编译与静态检查**

Run:
```bash
cd examples/receive-and-send
go build .
go vet .
```
Expected: 两条命令都无报错退出。

- [ ] **Step 9: 给 `main.go` 加配置与 wiring**

把 `Config` 结构体：

```go
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

改成：

```go
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
	TencentTTSVoiceType      int64  `yaml:"tencent_tts_voice_type"`
	COSBucket                string `yaml:"cos_bucket"`
	COSRegion                string `yaml:"cos_region"`
}
```

把 `main()` 里构造 `asrClient` 之后、构造 `Processor{}` 之前（紧跟在 Plan A 加的那段 `asrClient` 构造代码后面）插入：

```go
	// 留空 cos_bucket 则不启用语音回复功能，复用 ASR 已有的腾讯云密钥
	var ttsClient voiceReplySynthesizer
	var cosUploader voiceReplyUploader
	if config.COSBucket != "" && config.TencentSecretID != "" {
		t, err := NewTencentTTSClient(
			config.TencentSecretID, config.TencentSecretKey,
			config.TencentASRRegion, config.TencentTTSVoiceType,
		)
		if err != nil {
			log.Printf("[Voice] 创建腾讯云TTS客户端失败，语音回复功能不可用: %v", err)
		} else {
			c, err := NewCOSUploader(
				config.TencentSecretID, config.TencentSecretKey,
				config.COSBucket, config.COSRegion,
			)
			if err != nil {
				log.Printf("[Voice] 创建COS上传器失败，语音回复功能不可用: %v", err)
			} else {
				ttsClient = t
				cosUploader = c
				log.Printf("[Voice] 腾讯云TTS/COS已创建，语音回复功能已启用")
			}
		}
	}
```

把原来的：

```go
	processor = Processor{api: api, bailian: bailian, faq: faqEntries, memory: memory, asrClient: asrClient}
```

改成：

```go
	processor = Processor{
		api: api, bailian: bailian, faq: faqEntries, memory: memory,
		asrClient: asrClient, ttsClient: ttsClient, cosUploader: cosUploader,
	}
```

- [ ] **Step 10: 编译与静态检查**

Run:
```bash
cd examples/receive-and-send
go build .
go vet .
go test ./... -v
```
Expected: 三条命令都无报错，测试全部 `PASS`。

- [ ] **Step 11: 补本地 `config.yaml`（不提交 git）**

在本地 `examples/receive-and-send/config.yaml` 文件末尾追加（`tencent_tts_voice_type` 填的 `402001` 是腾讯云音色列表里"云小团"这个音色的 ID，确认支持 24000 采样率；如果想换别的音色去 [音色列表文档](https://cloud.tencent.com/document/product/1073/92668) 挑一个同样标注支持 24k 的即可。没有真实 COS bucket 时留空 `cos_bucket` 即可验证"未配置自动降级"分支）：

```yaml

# 腾讯云语音合成(TTS)+COS配置，用于把回复转成语音消息发给用户
# 留空 cos_bucket 则不启用语音回复功能
tencent_tts_voice_type: 402001
cos_bucket:
cos_region: ap-guangzhou
```

- [ ] **Step 12: 本地手工验证**

跟 Plan A Step 9 一样的流程：起本地 ngrok 隧道把 QQ 开放平台回调地址指向本机，私聊机器人发一条语音消息，观察：
- 启动日志里看到 `[Voice] 腾讯云TTS/COS已创建，语音回复功能已启用`
- 发语音后，QQ 客户端里**真的收到一条语音消息回复**（这是本计划风险最高的一步——`youthlin/silk` 编码出来的音频能不能被 QQ 客户端正常播放，只有这一步能验证到；如果放不出来或者是噪音，回去看设计文档里记录的 CGO 链接 `kn007/silk-v3-decoder` 备选方案）
- 把本地 `config.yaml` 的 `cos_bucket` 清空重启后再发一次语音，确认机器人改回发文字回复（降级分支生效），日志里能看到 `[Voice] 未配置TTS/COS...` 或 `TTS/COS未配置` 相关日志

- [ ] **Step 13: Commit**

```bash
cd examples/receive-and-send
git add asr.go asr_test.go process.go voice_reply.go voice_reply_test.go main.go
git commit -m "接入语音回复：私聊/群聊@场景语音提问时用语音回复，失败降级为文字"
```

---

## 完成后

Plan B（语音回复/TTS）到此完整可用：机器人在私聊/群聊@场景收到语音提问时，会尝试用语音回复，失败则降级为文字。至此语音消息功能（收+发）全部完成。建议完成后把 Task 4 Step 12 的手工验证结果（尤其是"QQ 客户端能不能正常播放"这条）记录进项目 memory，供后续参考。
