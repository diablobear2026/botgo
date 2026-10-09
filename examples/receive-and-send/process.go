package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/tencent-connect/botgo/dto"
	"github.com/tencent-connect/botgo/openapi"
)

// Processor is a struct to process message
type Processor struct {
	api       openapi.OpenAPI
	bailian   *BailianClient
	faq       []FAQEntry
	memory    *MemoryStore
	asrClient voiceTranscriber
}

// ProcessChannelMessage is a function to process message
func (p Processor) ProcessChannelMessage(input string, data *dto.WSATMessageData) error {
	// 频道场景暂不接入对话记忆
	msg := p.generateReplyMessage(input, dto.Message(*data), "")
	if err := p.sendChannelReply(context.Background(), data.ChannelID, msg); err != nil {
		_ = p.sendChannelReply(context.Background(), data.GroupID, genErrMessage(dto.Message(*data), err))
	}
	return nil
}

// ProcessInlineSearch is a function to process inline search
func (p Processor) ProcessInlineSearch(interaction *dto.WSInteractionData) error {
	if interaction.Data.Type != dto.InteractionDataTypeChatSearch {
		return fmt.Errorf("interaction data type not chat search")
	}
	search := &dto.SearchInputResolved{}
	if err := json.Unmarshal(interaction.Data.Resolved, search); err != nil {
		log.Println(err)
		return err
	}
	if search.Keyword != "test" {
		return fmt.Errorf("resolved search key not allowed")
	}
	searchRsp := &dto.SearchRsp{
		Layouts: []dto.SearchLayout{
			{
				LayoutType: 0,
				ActionType: 0,
				Title:      "内联搜索",
				Records: []dto.SearchRecord{
					{
						Cover: "https://pub.idqqimg.com/pc/misc/files/20211208/311cfc87ce394c62b7c9f0508658cf25.png",
						Title: "内联搜索标题",
						Tips:  "内联搜索 tips",
						URL:   "https://www.qq.com",
					},
				},
			},
		},
	}
	body, _ := json.Marshal(searchRsp)
	if err := p.api.PutInteraction(context.Background(), interaction.ID, string(body)); err != nil {
		log.Println("api call putInteractionInlineSearch  error: ", err)
		return err
	}
	return nil
}

func genErrMessage(data dto.Message, err error) *dto.MessageToCreate {
	return &dto.MessageToCreate{
		Timestamp: time.Now().UnixMilli(),
		Content:   fmt.Sprintf("处理异常:%v", err),
		MessageReference: &dto.MessageReference{
			// 引用这条消息
			MessageID:             data.ID,
			IgnoreGetMessageError: true,
		},
		MsgID: data.ID,
	}
}

// ProcessGroupMessage 回复群消息
func (p Processor) ProcessGroupMessage(input string, data *dto.WSGroupATMessageData) error {
	memberID := ""
	if data.Author != nil && data.Author.ID != "" {
		memberID = data.Author.ID
	}
	memKey := memoryKey("group", data.GroupID, memberID)
	msg := p.generateReplyMessage(input, dto.Message(*data), memKey)
	if err := p.sendGroupReply(context.Background(), data.GroupID, msg); err != nil {
		_ = p.sendGroupReply(context.Background(), data.GroupID, genErrMessage(dto.Message(*data), err))
	}

	return nil
}

// ProcessC2CMessage 回复C2C消息
func (p Processor) ProcessC2CMessage(input string, data *dto.WSC2CMessageData) error {
	userID := ""
	if data.Author != nil && data.Author.ID != "" {
		userID = data.Author.ID
	}
	memKey := memoryKey("c2c", "", userID)
	msg := p.generateReplyMessage(input, dto.Message(*data), memKey)
	if err := p.sendC2CReply(context.Background(), userID, msg); err != nil {
		_ = p.sendC2CReply(context.Background(), userID, genErrMessage(dto.Message(*data), err))
	}
	return nil
}

// generateReplyMessage 优先匹配固定 FAQ，命中则直接返回固定回复；未命中再调用百炼大模型。
// memKey 为空时不读写对话记忆（比如频道场景），非空时读取历史记忆一起发给大模型，回复后写回。
func (p Processor) generateReplyMessage(input string, data dto.Message, memKey string) *dto.MessageToCreate {
	log.Printf("收到指令: %+v", input)

	msg := "抱歉，我没有理解你的问题"
	if trimmed := strings.TrimSpace(input); trimmed != "" {
		if answer, ok := MatchFAQ(p.faq, trimmed); ok {
			log.Printf("命中 FAQ，跳过大模型调用")
			msg = answer
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			var history []ChatMessage
			if memKey != "" && p.memory != nil {
				if h, err := p.memory.LoadHistory(ctx, memKey); err != nil {
					log.Printf("读取对话记忆失败: %v", err)
				} else {
					history = h
				}
			}

			reply, err := p.bailian.Chat(ctx, history, trimmed)
			if err != nil {
				log.Printf("调用百炼接口失败: %v", err)
				msg = "抱歉，大模型服务暂时不可用，请稍后再试"
			} else {
				msg = reply
				if memKey != "" && p.memory != nil {
					if err := p.memory.AppendRound(ctx, memKey, trimmed, reply); err != nil {
						log.Printf("写入对话记忆失败: %v", err)
					}
				}
			}
		}
	}
	for _, _v := range data.Attachments {
		msg += ",收到文件类型:" + _v.ContentType
	}
	return &dto.MessageToCreate{
		Timestamp: time.Now().UnixMilli(),
		Content:   msg,
		MessageReference: &dto.MessageReference{
			// 引用这条消息
			MessageID:             data.ID,
			IgnoreGetMessageError: true,
		},
		MsgID: data.ID,
	}
}

// ProcessFriend 处理 c2c 好友事件
func (p Processor) ProcessFriend(wsEventType string, data *dto.WSC2CFriendData) error {
	// 请注意，这里是主动推送添加好友事件，后续改为 event id 被动消息
	replyMsg := dto.MessageToCreate{
		Timestamp: time.Now().UnixMilli(),
		Content:   "",
	}
	var content string
	switch strings.ToLower(wsEventType) {
	case strings.ToLower(string(dto.EventC2CFriendAdd)):
		log.Println("添加好友")
		content = fmt.Sprintf("ID为 %s 的用户添加机器人为好友", data.OpenID)
	case strings.ToLower(string(dto.EventC2CFriendDel)):
		log.Println("删除好友")
	default:
		log.Println(wsEventType)
		return nil
	}
	replyMsg.Content = content
	_, err := p.api.PostC2CMessage(
		context.Background(),
		data.OpenID,
		replyMsg,
	)
	if err != nil {
		log.Println(err)
		return err
	}
	return nil
}
