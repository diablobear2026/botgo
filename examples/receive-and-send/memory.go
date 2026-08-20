package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-redis/redis/v8"
)

// MemoryStore 基于 Redis 存储每个会话（私聊/群聊@）最近几轮的对话记忆
type MemoryStore struct {
	client *redis.Client
	rounds int
}

// NewMemoryStore 创建一个对话记忆存储，rounds 是保留的对话轮数（1 轮 = 1 条用户消息 + 1 条机器人回复）
func NewMemoryStore(addr, password string, db, rounds int) *MemoryStore {
	return &MemoryStore{
		client: redis.NewClient(&redis.Options{
			Addr:     addr,
			Password: password,
			DB:       db,
		}),
		rounds: rounds,
	}
}

// Ping 检测 Redis 连接是否正常，用于启动时打印明确的连接状态日志
func (s *MemoryStore) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// memoryKey 私聊按 userID(openid) 区分；群聊按 groupID+memberOpenID 区分，
// 避免同一个人在私聊和群聊、或者不同群里的记忆串在一起
func memoryKey(scene, groupID, userID string) string {
	if scene == "group" {
		return fmt.Sprintf("mem:group:%s:%s", groupID, userID)
	}
	return fmt.Sprintf("mem:c2c:%s", userID)
}

// LoadHistory 读取某个会话最近保留的对话记忆
func (s *MemoryStore) LoadHistory(ctx context.Context, key string) ([]ChatMessage, error) {
	raw, err := s.client.LRange(ctx, key, 0, -1).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	history := make([]ChatMessage, 0, len(raw))
	for _, item := range raw {
		var msg ChatMessage
		if err := json.Unmarshal([]byte(item), &msg); err != nil {
			continue
		}
		history = append(history, msg)
	}
	return history, nil
}

// AppendRound 追加一轮对话（用户输入 + 机器人回复），并裁剪到最近 rounds 轮
func (s *MemoryStore) AppendRound(ctx context.Context, key, userInput, assistantReply string) error {
	userMsg, err := json.Marshal(ChatMessage{Role: "user", Content: userInput})
	if err != nil {
		return err
	}
	assistantMsg, err := json.Marshal(ChatMessage{Role: "assistant", Content: assistantReply})
	if err != nil {
		return err
	}
	pipe := s.client.TxPipeline()
	pipe.RPush(ctx, key, userMsg, assistantMsg)
	pipe.LTrim(ctx, key, -int64(s.rounds*2), -1)
	_, err = pipe.Exec(ctx)
	return err
}
