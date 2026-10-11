package main

import (
	"encoding/base64"
	"testing"

	tts "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/tts/v20190823"
)

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
