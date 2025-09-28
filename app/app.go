package app

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/beck-8/subs-check/app/monitor"
	"github.com/beck-8/subs-check/assets"
	"github.com/beck-8/subs-check/check"
	"github.com/beck-8/subs-check/config"
	proxies "github.com/beck-8/subs-check/proxy"
	"github.com/beck-8/subs-check/save"
	"github.com/beck-8/subs-check/utils"
	"github.com/fsnotify/fsnotify"
	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// App 结构体用于管理应用程序状态
type App struct {
	configPath string
	interval   int
	watcher    *fsnotify.Watcher
	checkChan  chan struct{} // 触发检测的通道
	checking   atomic.Bool   // 检测状态标志
	ticker     *time.Ticker
	done       chan struct{} // 用于结束ticker goroutine的信号
	cron       *cron.Cron    // crontab调度器
	version    string
}

// New 创建新的应用实例
func New(version string) *App {
	configPath := flag.String("f", "", "配置文件路径")
	flag.Parse()

	return &App{
		configPath: *configPath,
		checkChan:  make(chan struct{}),
		done:       make(chan struct{}),
		version:    version,
	}
}

// Initialize 初始化应用程序
func (app *App) Initialize() error {
	// 初始化配置文件路径
	if err := app.initConfigPath(); err != nil {
		return fmt.Errorf("初始化配置文件路径失败: %w", err)
	}

	// 加载配置文件
	if err := app.loadConfig(); err != nil {
		return fmt.Errorf("加载配置文件失败: %w", err)
	}

	// 初始化配置文件监听
	if err := app.initConfigWatcher(); err != nil {
		return fmt.Errorf("初始化配置文件监听失败: %w", err)
	}

	// 从配置文件中读取代理，设置代理
	if config.GlobalConfig.Proxy != "" {
		os.Setenv("HTTP_PROXY", config.GlobalConfig.Proxy)
		os.Setenv("HTTPS_PROXY", config.GlobalConfig.Proxy)
	}

	app.interval = config.GlobalConfig.CheckInterval

	if config.GlobalConfig.ListenPort != "" {
		if err := app.initHttpServer(); err != nil {
			return fmt.Errorf("初始化HTTP服务器失败: %w", err)
		}
	}

	if config.GlobalConfig.SubStorePort != "" {
		if runtime.GOOS == "linux" && runtime.GOARCH == "386" {
			slog.Warn("node不支持Linux 32位系统，不启动sub-store服务")
		}
		go assets.RunSubStoreService()
		// 求等吗得，日志会按预期顺序输出
		time.Sleep(500 * time.Millisecond)
	}

	// 启动内存监控
	monitor.StartMemoryMonitor()

	// 设置信号处理器
	utils.SetupSignalHandler(&check.ForceClose)
	return nil
}

// Run 运行应用程序主循环
func (app *App) Run() {
	defer func() {
		app.watcher.Close()
		if app.ticker != nil {
			app.ticker.Stop()
		}
		if app.cron != nil {
			app.cron.Stop()
		}
	}()

	// 设置初始定时器模式
	app.setTimer()

	// 仅在cron表达式为空时，首次启动立即执行检测
	if config.GlobalConfig.CronExpression != "" {
		slog.Warn("使用cron表达式，首次启动不立即执行检测")
	} else {
		app.triggerCheck()
	}

	// 在主循环中处理手动触发
	for range app.checkChan {
		go app.triggerCheck()
	}
}

// setTimer 根据配置设置定时器
func (app *App) setTimer() {
	// 停止现有定时器
	if app.ticker != nil {
		// 应该先发送停止信号，防止被=nil后panic
		close(app.done)                // 发送停止信号
		app.done = make(chan struct{}) // 创建新通道
		app.ticker.Stop()
		app.ticker = nil
	}

	// 停止现有cron
	if app.cron != nil {
		app.cron.Stop()
		app.cron = nil
	}

	// 检查是否设置了cron表达式
	if config.GlobalConfig.CronExpression != "" {
		slog.Info(fmt.Sprintf("使用cron表达式: %s", config.GlobalConfig.CronExpression))
		app.cron = cron.New()
		_, err := app.cron.AddFunc(config.GlobalConfig.CronExpression, func() {
			app.triggerCheck()
		})
		if err != nil {
			slog.Error(fmt.Sprintf("cron表达式 '%s' 解析失败: %v，将使用检查间隔时间",
				config.GlobalConfig.CronExpression, err))
			// 使用间隔时间
			app.useIntervalTimer()
		} else {
			app.cron.Start()
		}
	} else {
		// 使用间隔时间
		app.useIntervalTimer()
	}
}

// useIntervalTimer 使用间隔时间模式运行
func (app *App) useIntervalTimer() {
	// 初始化定时器
	app.ticker = time.NewTicker(time.Duration(app.interval) * time.Minute)
	done := app.done
	// 启动一个goroutine监听定时器事件
	go func() {
		for {
			select {
			case <-app.ticker.C:
				app.triggerCheck()
			case <-done:
				return // 收到停止信号，退出goroutine
			}
		}
	}()
}

// TriggerCheck 供外部调用的触发检测方法
func (app *App) TriggerCheck() {
	select {
	case app.checkChan <- struct{}{}:
		slog.Info("手动触发检测")
	default:
		slog.Warn("已有检测正在进行，忽略本次触发")
	}
}

// triggerCheck 内部检测方法
func (app *App) triggerCheck() {
	// 如果已经在检测中，直接返回
	if !app.checking.CompareAndSwap(false, true) {
		slog.Warn("已有检测正在进行，跳过本次检测")
		return
	}
	defer app.checking.Store(false)

	if err := app.checkProxies(); err != nil {
		slog.Error(fmt.Sprintf("检测代理失败: %v", err))
		os.Exit(1)
	}

	// 检测完成后显示下次检查时间
	if app.ticker != nil {
		// 使用间隔时间模式
		app.ticker.Reset(time.Duration(app.interval) * time.Minute)
		nextCheck := time.Now().Add(time.Duration(app.interval) * time.Minute)
		slog.Info(fmt.Sprintf("下次检查时间: %s", nextCheck.Format("2006-01-02 15:04:05")))
	} else if app.cron != nil {
		// 使用cron模式
		entries := app.cron.Entries()
		if len(entries) > 0 {
			nextTime := entries[0].Next
			slog.Info(fmt.Sprintf("下次检查时间: %s", nextTime.Format("2006-01-02 15:04:05")))
		}
	}
	debug.FreeOSMemory()
}

// checkProxies 执行代理检测
func (app *App) checkProxies() error {
	slog.Info("开始准备检测代理", "进度展示", config.GlobalConfig.PrintProgress)

	results, err := check.Check()
	if err != nil {
		return fmt.Errorf("检测代理失败: %w", err)
	}
	// 将成功的节点添加到全局中，暂时内存保存
	if config.GlobalConfig.KeepSuccessProxies {
		for _, result := range results {
			if result.Proxy != nil {
				config.GlobalProxies = append(config.GlobalProxies, result.Proxy)
			}
		}
	}

	slog.Info("检测完成")
	save.SaveConfig(results)
	utils.SendNotify(len(results))
	utils.UpdateSubs()

	// 执行回调脚本
	utils.ExecuteCallback(len(results))

	// 处理订阅生命周期管理
	if err := app.handleSubsLifecycle(); err != nil {
		slog.Error("处理订阅生命周期失败", "error", err)
	}

	return nil
}

// handleSubsLifecycle 处理订阅生命周期管理
func (app *App) handleSubsLifecycle() error {
	// 如果功能被禁用，直接返回
	if config.GlobalConfig.SubUrlsFailRemove <= 0 {
		return nil
	}

	// 获取本轮统计
	successUrls, failedUrls := proxies.GetAndResetRunStats()

	if len(successUrls) == 0 && len(failedUrls) == 0 {
		return nil // 没有统计数据
	}

	// 获取配置目录
	configDir := filepath.Dir(app.configPath)

	// 加载状态文件
	state, err := LoadSubsState(configDir)
	if err != nil {
		return fmt.Errorf("加载订阅状态失败: %w", err)
	}

	// 更新失败计数
	for _, url := range successUrls {
		state.UpdateFailCount(url, false) // 成功
	}
	for _, url := range failedUrls {
		state.UpdateFailCount(url, true) // 失败
	}

	// 获取需要移除的URL
	urlsToRemove := state.GetFailedUrls(config.GlobalConfig.SubUrlsFailRemove)

	if len(urlsToRemove) > 0 {
		slog.Warn("发现需要移除的订阅", "count", len(urlsToRemove), "urls", urlsToRemove)

		// 从配置文件中移除这些URL
		if err := app.removeUrlsFromConfig(urlsToRemove); err != nil {
			return fmt.Errorf("从配置文件移除URL失败: %w", err)
		}

		// 清理状态记录
		state.CleanupUrls(urlsToRemove)

		slog.Info("已自动移除失败订阅", "count", len(urlsToRemove))
	}

	// 保存状态文件
	if err := state.SaveToFile(configDir); err != nil {
		return fmt.Errorf("保存订阅状态失败: %w", err)
	}

	return nil
}

// removeUrlsFromConfig 从配置文件中移除指定的URL
func (app *App) removeUrlsFromConfig(urlsToRemove []string) error {
	// 读取配置文件
	yamlFile, err := os.ReadFile(app.configPath)
	if err != nil {
		return fmt.Errorf("读取配置文件失败: %w", err)
	}

	// 解析为map以保持格式
	var yamlData map[string]any
	if err := yaml.Unmarshal(yamlFile, &yamlData); err != nil {
		return fmt.Errorf("解析配置文件失败: %w", err)
	}

	// 获取sub-urls数组
	subUrlsInterface, exists := yamlData["sub-urls"]
	if !exists {
		return nil // 没有sub-urls字段，无需处理
	}

	subUrls, ok := subUrlsInterface.([]any)
	if !ok {
		return fmt.Errorf("sub-urls字段格式错误")
	}

	// 创建要移除的URL集合
	removeSet := make(map[string]bool)
	for _, url := range urlsToRemove {
		removeSet[url] = true
	}

	// 过滤掉要移除的URL
	var filteredUrls []any
	removedCount := 0
	for _, urlInterface := range subUrls {
		if urlStr, ok := urlInterface.(string); ok {
			if !removeSet[urlStr] {
				filteredUrls = append(filteredUrls, urlInterface)
			} else {
				removedCount++
			}
		} else {
			filteredUrls = append(filteredUrls, urlInterface)
		}
	}

	if removedCount == 0 {
		return nil // 没有URL被移除
	}

	// 更新配置
	yamlData["sub-urls"] = filteredUrls

	// 序列化回YAML
	updatedYaml, err := yaml.Marshal(yamlData)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	// 写回文件
	if err := os.WriteFile(app.configPath, updatedYaml, 0644); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}

	slog.Info("已从配置文件移除订阅", "removed", removedCount, "remaining", len(filteredUrls))
	return nil
}

func TempLog() string {
	return filepath.Join(os.TempDir(), "subs-check.log")
}
