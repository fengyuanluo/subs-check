package app

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// SubsState 订阅状态管理
type SubsState struct {
	FailCounts map[string]int `json:"fail_counts"`
}

// NewSubsState 创建新的订阅状态
func NewSubsState() *SubsState {
	return &SubsState{
		FailCounts: make(map[string]int),
	}
}

// LoadSubsState 从文件加载订阅状态
func LoadSubsState(configDir string) (*SubsState, error) {
	statePath := filepath.Join(configDir, "subs_state.json")
	
	// 如果文件不存在，返回新的状态
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		slog.Debug("订阅状态文件不存在，创建新状态")
		return NewSubsState(), nil
	}
	
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("读取订阅状态文件失败: %w", err)
	}
	
	var state SubsState
	if err := json.Unmarshal(data, &state); err != nil {
		slog.Warn("解析订阅状态文件失败，创建新状态", "error", err)
		return NewSubsState(), nil
	}
	
	// 确保 FailCounts 不为 nil
	if state.FailCounts == nil {
		state.FailCounts = make(map[string]int)
	}
	
	return &state, nil
}

// SaveToFile 保存状态到文件
func (s *SubsState) SaveToFile(configDir string) error {
	statePath := filepath.Join(configDir, "subs_state.json")
	
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化订阅状态失败: %w", err)
	}
	
	if err := os.WriteFile(statePath, data, 0644); err != nil {
		return fmt.Errorf("写入订阅状态文件失败: %w", err)
	}
	
	return nil
}

// UpdateFailCount 更新订阅的失败计数
func (s *SubsState) UpdateFailCount(url string, failed bool) {
	if failed {
		s.FailCounts[url]++
		slog.Debug("订阅失败计数更新", "url", url, "count", s.FailCounts[url])
	} else {
		// 成功时清零计数
		if s.FailCounts[url] > 0 {
			slog.Debug("订阅成功，清零失败计数", "url", url)
		}
		s.FailCounts[url] = 0
	}
}

// GetFailedUrls 获取失败次数超过阈值的URL列表
func (s *SubsState) GetFailedUrls(threshold int) []string {
	if threshold <= 0 {
		return nil
	}
	
	var failedUrls []string
	for url, count := range s.FailCounts {
		if count >= threshold {
			failedUrls = append(failedUrls, url)
		}
	}
	
	return failedUrls
}

// CleanupUrls 清理指定URL的状态记录
func (s *SubsState) CleanupUrls(urls []string) {
	for _, url := range urls {
		delete(s.FailCounts, url)
		slog.Debug("清理订阅状态记录", "url", url)
	}
}

// GetFailCount 获取指定URL的失败次数
func (s *SubsState) GetFailCount(url string) int {
	return s.FailCounts[url]
}

// GetTotalUrls 获取状态中记录的URL总数
func (s *SubsState) GetTotalUrls() int {
	return len(s.FailCounts)
}
