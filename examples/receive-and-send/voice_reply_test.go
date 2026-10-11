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
