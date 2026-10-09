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
