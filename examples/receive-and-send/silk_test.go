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
