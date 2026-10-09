package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	asr "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/asr/v20190614"
	"github.com/tencent-connect/botgo/dto"
)

func strPtr(s string) *string { return &s }

func TestExtractRecognizedText(t *testing.T) {
	cases := []struct {
		name    string
		resp    *asr.SentenceRecognitionResponse
		want    string
		wantErr bool
	}{
		{name: "nil response", resp: nil, wantErr: true},
		{name: "nil Response字段", resp: &asr.SentenceRecognitionResponse{}, wantErr: true},
		{
			name:    "nil Result",
			resp:    &asr.SentenceRecognitionResponse{Response: &asr.SentenceRecognitionResponseParams{}},
			wantErr: true,
		},
		{
			name:    "空字符串Result",
			resp:    &asr.SentenceRecognitionResponse{Response: &asr.SentenceRecognitionResponseParams{Result: strPtr("")}},
			wantErr: true,
		},
		{
			name:    "只有空格的Result",
			resp:    &asr.SentenceRecognitionResponse{Response: &asr.SentenceRecognitionResponseParams{Result: strPtr("   ")}},
			wantErr: true,
		},
		{
			name: "正常识别结果(带前后空格需要trim)",
			resp: &asr.SentenceRecognitionResponse{
				Response: &asr.SentenceRecognitionResponseParams{Result: strPtr(" 今天天气怎么样 ")},
			},
			want: "今天天气怎么样",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := extractRecognizedText(c.resp)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望返回错误，实际返回 nil (text=%q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不期望报错，实际报错: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestFindVoiceAttachment(t *testing.T) {
	voice := &dto.MessageAttachment{ContentType: "voice", URL: "https://example.com/a.silk"}
	image := &dto.MessageAttachment{ContentType: "image/png", URL: "https://example.com/b.png"}

	cases := []struct {
		name        string
		attachments []*dto.MessageAttachment
		want        *dto.MessageAttachment
	}{
		{name: "nil切片", attachments: nil, want: nil},
		{name: "空切片", attachments: []*dto.MessageAttachment{}, want: nil},
		{name: "没有语音附件", attachments: []*dto.MessageAttachment{image}, want: nil},
		{name: "语音附件混在其他附件里", attachments: []*dto.MessageAttachment{image, voice}, want: voice},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := findVoiceAttachment(c.attachments)
			if got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestDownloadVoiceBytes(t *testing.T) {
	t.Run("下载成功", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("fake-silk-bytes"))
		}))
		defer srv.Close()

		data, err := downloadVoiceBytes(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("不期望报错: %v", err)
		}
		if string(data) != "fake-silk-bytes" {
			t.Fatalf("got %q", string(data))
		}
	})

	t.Run("非200状态码", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		if _, err := downloadVoiceBytes(context.Background(), srv.URL); err == nil {
			t.Fatal("期望报错，实际没有报错")
		}
	})

	t.Run("超过大小限制", func(t *testing.T) {
		big := bytes.Repeat([]byte("a"), maxVoiceBytes+1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(big)
		}))
		defer srv.Close()

		if _, err := downloadVoiceBytes(context.Background(), srv.URL); err == nil {
			t.Fatal("期望报错，实际没有报错")
		}
	})
}

type fakeTranscriber struct {
	text string
	err  error
}

func (f fakeTranscriber) Transcribe(_ context.Context, _ []byte) (string, error) {
	return f.text, f.err
}

func TestResolveUserInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fake-silk-bytes"))
	}))
	defer srv.Close()
	voiceAttachments := []*dto.MessageAttachment{{ContentType: "voice", URL: srv.URL}}

	t.Run("没有语音附件，原样返回文字输入", func(t *testing.T) {
		p := Processor{asrClient: fakeTranscriber{text: "不应该被调用"}}
		got, needFallback := p.resolveUserInput(context.Background(), "你好", nil)
		if needFallback || got != "你好" {
			t.Fatalf("got (%q,%v)", got, needFallback)
		}
	})

	t.Run("未配置ASR客户端", func(t *testing.T) {
		p := Processor{asrClient: nil}
		_, needFallback := p.resolveUserInput(context.Background(), "", voiceAttachments)
		if !needFallback {
			t.Fatal("期望 needFallback=true")
		}
	})

	t.Run("识别成功", func(t *testing.T) {
		p := Processor{asrClient: fakeTranscriber{text: "今天天气怎么样"}}
		got, needFallback := p.resolveUserInput(context.Background(), "", voiceAttachments)
		if needFallback || got != "今天天气怎么样" {
			t.Fatalf("got (%q,%v)", got, needFallback)
		}
	})

	t.Run("识别失败", func(t *testing.T) {
		p := Processor{asrClient: fakeTranscriber{err: errors.New("boom")}}
		_, needFallback := p.resolveUserInput(context.Background(), "", voiceAttachments)
		if !needFallback {
			t.Fatal("期望 needFallback=true")
		}
	})
}

func TestGenVoiceFallbackMessage(t *testing.T) {
	msg := genVoiceFallbackMessage(dto.Message{ID: "msg-123"})
	if msg.MsgID != "msg-123" {
		t.Fatalf("got MsgID=%q", msg.MsgID)
	}
	if msg.Content == "" {
		t.Fatal("期望兜底文案非空")
	}
}
