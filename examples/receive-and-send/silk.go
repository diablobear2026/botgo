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
