package main

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// FAQEntry 是一条固定问答规则：输入命中任一关键词，就返回对应的固定回复
type FAQEntry struct {
	Keywords []string `yaml:"keywords"`
	Answer   string   `yaml:"answer"`
}

// LoadFAQ 从 yaml 文件加载 FAQ 列表；文件不存在时视为没有配置 FAQ，不报错
func LoadFAQ(path string) ([]FAQEntry, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var entries []FAQEntry
	if err := yaml.Unmarshal(content, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// MatchFAQ 按顺序检查每条 FAQ，输入包含其任一关键词（忽略大小写）即命中，返回固定回复
func MatchFAQ(entries []FAQEntry, input string) (string, bool) {
	lowerInput := strings.ToLower(strings.TrimSpace(input))
	if lowerInput == "" {
		return "", false
	}
	for _, entry := range entries {
		for _, keyword := range entry.Keywords {
			keyword = strings.ToLower(strings.TrimSpace(keyword))
			if keyword == "" {
				continue
			}
			if strings.Contains(lowerInput, keyword) {
				return entry.Answer, true
			}
		}
	}
	return "", false
}
