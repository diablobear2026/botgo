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
