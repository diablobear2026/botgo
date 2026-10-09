package main

import (
	"testing"

	asr "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/asr/v20190614"
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
