package main

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/youthlin/silk"
)

// EncodeToSilk 把PCM裸音频（16位有符号小端，24000采样率，单声道）编码成silk v3格式，
// 用的是纯Go库(github.com/youthlin/silk)，不涉及CGO。
//
// 必须显式传 Stx(true)：这个库的Stx选项默认是false（标准silk格式，开头直接是"#!SILK_V3"）。
// 但QQ/微信要求语音消息文件开头带一个0x02(STX)字节再接"#!SILK_V3"，否则QQ会把文件当成普通附件
// 而不是语音消息处理——这不是锦上添花的选项，是QQ语音消息能不能被正确识别的硬要求。
func EncodeToSilk(pcm []byte) ([]byte, error) {
	if len(pcm) == 0 {
		return nil, errors.New("PCM输入为空")
	}
	silkBytes, err := silk.Encode(bytes.NewReader(pcm), silk.SampleRate(24000), silk.Stx(true))
	if err != nil {
		return nil, fmt.Errorf("silk编码失败: %w", err)
	}
	return silkBytes, nil
}
