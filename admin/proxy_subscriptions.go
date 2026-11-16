package admin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/codex2api/database"
	"github.com/codex2api/managedproxy"
	"github.com/gin-gonic/gin"
)

// The subscription manager owns only its private core and listener URLs. Existing
// hand-entered proxies and operator enable/disable decisions are not overwritten.
type subscriptionManager interface {
	View() managedproxy.View
	Configure(context.Context, managedproxy.EngineConfig) error
	SaveSubscription(context.Context, managedproxy.Subscription) (managedproxy.Subscription, error)
	DeleteSubscription(context.Context, string) error
	Refresh(context.Context, string) error
	Ensure(context.Context) error
	Close() error
	SetChangeHook(func())
}

type proxySubscriptionOperation struct {
	Running    bool   `json:"running"`
	Stage      string `json:"stage"`
	Error      string `json:"error"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
}

type proxySubscriptionRuntime struct {
	manager   subscriptionManager
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	syncMu    sync.Mutex
	wg        sync.WaitGroup
	operation proxySubscriptionOperation
	syncError string
	closed    bool
}

func (h *Handler) StartProxySubscriptions(parent context.Context, directory string) error {
	manager, err := managedproxy.New(directory)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	runtime := &proxySubscriptionRuntime{manager: manager, ctx: ctx, cancel: cancel}
	h.proxySubscriptions = runtime
	manager.SetChangeHook(func() {
		syncCtx, stop := context.WithTimeout(ctx, time.Minute)
		defer stop()
		err := h.syncSubscriptionProxies(syncCtx)
		runtime.mu.Lock()
		runtime.syncError = ""
		if err != nil {
			runtime.syncError = "订阅配置已应用，但代理池同步失败，请重试刷新"
		}
		runtime.mu.Unlock()
	})
	// Do not install or launch a core on instances that have never enabled a feed.
	// A container recreate leaves no child process behind while the enabled feed
	// and its listeners stay configured, so startup must retry until the core is
	// actually serving. Bounded retries keep a broken binary from looping.
	for _, subscription := range manager.View().Subscriptions {
		if subscription.Enabled {
			go h.autostartProxyEngine(runtime, manager, backgroundRetryDelay)
			break
		}
	}
	return nil
}

// backgroundRetryDelay is a test seam for the restore loop; production waits a
// few seconds so the core can bind its listeners before the first retry.
const backgroundRetryDelay = 3 * time.Second
const backgroundRetryLimit = 10

// autostartProxyEngine restores a previously enabled managed core after a
// container recreate. It retries with a bounded budget and reports every failed
// round, so a broken install is visible instead of silently leaving dead
// loopback listeners behind.
func (h *Handler) autostartProxyEngine(runtime *proxySubscriptionRuntime, manager subscriptionManager, retryDelay time.Duration) {
	for attempt := 1; attempt <= backgroundRetryLimit; attempt++ {
		view := manager.View()
		if view.Running {
			log.Printf("订阅代理引擎已在运行: version=%s", view.Version)
			return
		}
		err := func() error {
			ctx, cancel := context.WithTimeout(runtime.ctx, 5*time.Minute)
			defer cancel()
			if ensureErr := manager.Ensure(ctx); ensureErr != nil {
				return ensureErr
			}
			return h.syncSubscriptionProxies(ctx)
		}()
		if err == nil {
			log.Printf("订阅代理引擎自动恢复成功: attempt=%d", attempt)
			return
		}
		runtime.mu.Lock()
		runtime.syncError = "订阅配置已应用，但代理池同步失败，请重试刷新"
		runtime.mu.Unlock()
		log.Printf("订阅代理引擎自动恢复失败: attempt=%d error=%v", attempt, err)
		if attempt == backgroundRetryLimit || runtime.ctx.Err() != nil {
			log.Printf("订阅代理引擎自动恢复放弃: attempts=%d", attempt)
			return
		}
		select {
		case <-runtime.ctx.Done():
			return
		case <-time.After(retryDelay):
		}
	}
}

func (h *Handler) CloseProxySubscriptions() error {
	runtime := h.proxySubscriptions
	if runtime == nil {
		return nil
	}
	runtime.mu.Lock()
	runtime.closed = true
	runtime.cancel()
	runtime.mu.Unlock()
	// Close interrupts managed downloads/children; never touches external cores.
	err := runtime.manager.Close()
	runtime.wg.Wait()
	return err
}

func (runtime *proxySubscriptionRuntime) start(stage string, work func(context.Context) error) bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.closed || runtime.operation.Running {
		return false
	}
	runtime.operation = proxySubscriptionOperation{Running: true, Stage: stage, StartedAt: time.Now().UTC().Format(time.RFC3339)}
	runtime.wg.Add(1)
	go func() {
		defer runtime.wg.Done()
		ctx, cancel := context.WithTimeout(runtime.ctx, 5*time.Minute)
		defer cancel()
		err := work(ctx)
		runtime.mu.Lock()
		runtime.operation.Running = false
		runtime.operation.Stage = "completed"
		runtime.operation.Error = ""
		if err != nil {
			runtime.operation.Stage = "failed"
			runtime.operation.Error = err.Error()
		}
		runtime.operation.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		runtime.mu.Unlock()
	}()
	return true
}

func (h *Handler) subscriptionRuntime(c *gin.Context) *proxySubscriptionRuntime {
	if h.proxySubscriptions == nil {
		writeError(c, http.StatusServiceUnavailable, "订阅管理未初始化，请检查数据目录权限及服务日志")
		return nil
	}
	return h.proxySubscriptions
}

func (h *Handler) ListProxySubscriptions(c *gin.Context) {
	runtime := h.subscriptionRuntime(c)
	if runtime == nil {
		return
	}
	view := runtime.manager.View()
	view.Subscriptions = append([]managedproxy.Subscription{}, view.Subscriptions...)
	for i := range view.Subscriptions {
		view.Subscriptions[i].URL = ""
	}
	runtime.mu.Lock()
	operation, syncError := runtime.operation, runtime.syncError
	runtime.mu.Unlock()
	if syncError != "" {
		view.LastError = syncError
	}
	c.JSON(http.StatusOK, struct {
		managedproxy.View
		Operation proxySubscriptionOperation `json:"operation"`
	}{View: view, Operation: operation})
}

// mutateSubscription reserves the same operation gate as refresh, including
// short config writes, so a save cannot race an in-flight candidate generation.
func (runtime *proxySubscriptionRuntime) mutateSubscription(work func() error) error {
	runtime.mu.Lock()
	if runtime.closed || runtime.operation.Running {
		runtime.mu.Unlock()
		return errProxySubscriptionBusy
	}
	runtime.operation.Running = true
	runtime.operation.Stage = "saving"
	runtime.mu.Unlock()
	err := work()
	runtime.mu.Lock()
	runtime.operation.Running = false
	runtime.operation.Stage = "completed"
	runtime.operation.Error = ""
	if err != nil {
		runtime.operation.Stage = "failed"
		runtime.operation.Error = err.Error()
	}
	runtime.mu.Unlock()
	return err
}

var errProxySubscriptionBusy = errors.New("订阅管理任务正在运行，请等待完成")

func subscriptionWriteError(c *gin.Context, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, errProxySubscriptionBusy) {
		status = http.StatusConflict
	}
	writeError(c, status, err.Error())
}

func (h *Handler) SaveProxySubscription(c *gin.Context) {
	runtime := h.subscriptionRuntime(c)
	if runtime == nil {
		return
	}
	var subscription managedproxy.Subscription
	if err := c.ShouldBindJSON(&subscription); err != nil {
		writeError(c, 400, "请求格式错误")
		return
	}
	if id := c.Param("id"); id != "" {
		subscription.ID = id
	} else {
		subscription.ID = ""
	}
	var saved managedproxy.Subscription
	err := runtime.mutateSubscription(func() error {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
		defer cancel()
		var err error
		saved, err = runtime.manager.SaveSubscription(ctx, subscription)
		return err
	})
	if err != nil {
		subscriptionWriteError(c, err)
		return
	}
	// Never echo the supplied bearer subscription URL in API responses.
	saved.URL = ""
	c.JSON(http.StatusOK, saved)
}

func (h *Handler) ConfigureProxyEngine(c *gin.Context) {
	runtime := h.subscriptionRuntime(c)
	if runtime == nil {
		return
	}
	var config managedproxy.EngineConfig
	if err := c.ShouldBindJSON(&config); err != nil {
		writeError(c, 400, "请求格式错误")
		return
	}
	err := runtime.mutateSubscription(func() error {
		return runtime.manager.Configure(c.Request.Context(), config)
	})
	if err != nil {
		subscriptionWriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) RefreshProxySubscriptions(c *gin.Context) {
	runtime := h.subscriptionRuntime(c)
	if runtime == nil {
		return
	}
	id := c.Param("id")
	if !runtime.start("refreshing", func(ctx context.Context) error {
		if err := runtime.manager.Refresh(ctx, id); err != nil {
			return err
		}
		if err := runtime.manager.Ensure(ctx); err != nil {
			return err
		}
		runtime.mu.Lock()
		runtime.operation.Stage = "reconciling"
		runtime.mu.Unlock()
		return h.syncSubscriptionProxies(ctx)
	}) {
		writeError(c, 409, errProxySubscriptionBusy.Error())
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"accepted": true})
}

func (h *Handler) StartProxyEngine(c *gin.Context) {
	runtime := h.subscriptionRuntime(c)
	if runtime == nil {
		return
	}
	if !runtime.start("starting", func(ctx context.Context) error {
		if err := runtime.manager.Ensure(ctx); err != nil {
			return err
		}
		return h.syncSubscriptionProxies(ctx)
	}) {
		writeError(c, 409, errProxySubscriptionBusy.Error())
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"accepted": true})
}

func (h *Handler) DeleteProxySubscription(c *gin.Context) {
	runtime := h.subscriptionRuntime(c)
	if runtime == nil {
		return
	}
	id := c.Param("id")
	if !runtime.start("deleting", func(ctx context.Context) error {
		if err := runtime.manager.DeleteSubscription(ctx, id); err != nil {
			return err
		}
		return h.syncSubscriptionProxies(ctx)
	}) {
		writeError(c, 409, errProxySubscriptionBusy.Error())
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"accepted": true})
}

func subscriptionProxyLabel(subscription, node string) string {
	label := strings.TrimSpace(subscription + " / " + node)
	for utf8.RuneCountInString(label) > 255 {
		_, size := utf8.DecodeLastRuneInString(label)
		label = label[:len(label)-size]
	}
	return label
}

func (h *Handler) syncSubscriptionProxies(ctx context.Context) error {
	runtime := h.proxySubscriptions
	if runtime == nil || h.db == nil {
		return errors.New("订阅代理存储不可用")
	}
	runtime.syncMu.Lock()
	defer runtime.syncMu.Unlock()
	view := runtime.manager.View()
	if !view.Running {
		for _, subscription := range view.Subscriptions {
			if subscription.Enabled {
				return errors.New("mihomo 尚未运行，保留原代理池状态")
			}
		}
	}
	rows, err := h.db.ListProxies(ctx)
	if err != nil {
		return errors.New("读取代理池失败")
	}
	byURL := make(map[string]*database.ProxyRow, len(rows))
	for _, row := range rows {
		if row != nil {
			byURL[strings.TrimSpace(row.URL)] = row
		}
	}
	names := make(map[string]string, len(view.Subscriptions))
	for _, subscription := range view.Subscriptions {
		names[subscription.ID] = subscription.Name
	}
	var removed []string
	for _, node := range view.Nodes {
		row := byURL[node.URL]
		if !node.Active {
			if row != nil && row.Enabled {
				enabled := false
				if err := h.db.UpdateProxy(ctx, row.ID, nil, nil, &enabled); err != nil {
					return errors.New("停用过期订阅节点失败")
				}
				removed = append(removed, node.URL)
			}
			continue
		}
		if !view.Running {
			return errors.New("mihomo 尚未运行，未发布新订阅节点")
		}
		if row != nil {
			continue
		} // Respect operator labels, health and enabled state.
		label := subscriptionProxyLabel(names[node.SubscriptionID], node.Name)
		if _, err := h.db.InsertProxy(ctx, node.URL, label); err != nil {
			return fmt.Errorf("导入订阅节点失败")
		}
	}
	if h.store != nil && len(removed) > 0 {
		h.removeProxyURLsFromRuntime(removed)
	}
	if err := h.reloadProxyPool(); err != nil {
		return errors.New("订阅节点已保存，但刷新代理池失败")
	}
	return nil
}

func (h *Handler) subscriptionOwnsProxyURL(rawURL string) bool {
	if h.proxySubscriptions == nil {
		return false
	}
	for _, node := range h.proxySubscriptions.manager.View().Nodes {
		if strings.TrimSpace(rawURL) == node.URL {
			return true
		}
	}
	return false
}
