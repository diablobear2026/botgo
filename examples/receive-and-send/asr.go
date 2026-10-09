package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	asr "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/asr/v20190614"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	"github.com/tencent-connect/botgo/dto"
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
