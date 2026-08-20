package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// 百炼(DashScope)OpenAI 兼容模式接口地址
const bailianChatCompletionsURL = "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"

// BailianClient 封装对阿里云百炼大模型的调用
type BailianClient struct {
	APIKey     string
	Model      string
	httpClient *http.Client
}

// NewBailianClient 创建一个百炼客户端
func NewBailianClient(apiKey, model string) *BailianClient {
	if model == "" {
		model = "qwen-plus"
	}
	return &BailianClient{
		APIKey: apiKey,
		Model:  model,
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

// ChatMessage 表示一条对话消息，既是百炼请求/响应的消息格式，也是对话记忆的存储格式
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type bailianChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
}

type bailianChatResponse struct {
	Choices []struct {
		Message ChatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

// Chat 发送一段用户输入给百炼大模型，history 是之前几轮的对话记忆（可为空），返回回复文本
func (c *BailianClient) Chat(ctx context.Context, history []ChatMessage, userInput string) (string, error) {
	if c.APIKey == "" {
		return "", errors.New("bailian api key 未配置")
	}
	messages := make([]ChatMessage, 0, len(history)+2)
	messages = append(messages, ChatMessage{Role: "system", Content: "你是一个部署在 QQ 机器人里的助手，请用简洁自然的中文回答用户问题。"})
	messages = append(messages, history...)
	messages = append(messages, ChatMessage{Role: "user", Content: userInput})
	reqBody := bailianChatRequest{
		Model:    c.Model,
		Messages: messages,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("序列化请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, bailianChatCompletionsURL, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求百炼接口失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}

	var result bailianChatResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("解析响应失败: %w, body: %s", err, string(body))
	}

	if resp.StatusCode != http.StatusOK {
		if result.Error != nil {
			return "", fmt.Errorf("百炼接口返回错误(%s): %s", result.Error.Code, result.Error.Message)
		}
		return "", fmt.Errorf("百炼接口返回非 200 状态码: %d, body: %s", resp.StatusCode, string(body))
	}

	if len(result.Choices) == 0 {
		return "", errors.New("百炼接口未返回任何回复")
	}

	return result.Choices[0].Message.Content, nil
}
