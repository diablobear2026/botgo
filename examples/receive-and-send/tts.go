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
