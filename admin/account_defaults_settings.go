package admin

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type accountDefaultsResponse struct {
	database.AccountDefaultsConfig
	PlanChoices map[string][]string `json:"plan_choices"`
}

func (h *Handler) accountDefaultsResponse(cfg database.AccountDefaultsConfig) accountDefaultsResponse {
	if cfg.ConcurrencyRules == nil {
		cfg.ConcurrencyRules = []database.AccountConcurrencyRule{}
	}
	return accountDefaultsResponse{AccountDefaultsConfig: cfg, PlanChoices: h.store.AccountConcurrencyPlanChoices()}
}

func (h *Handler) GetAccountDefaults(c *gin.Context) {
	cfg, err := h.db.LoadAccountDefaultsConfig(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取账号默认值失败")
		return
	}
	c.JSON(http.StatusOK, h.accountDefaultsResponse(cfg))
}

func (h *Handler) UpdateAccountDefaults(c *gin.Context) {
	var req struct {
		ConcurrencyRules      *[]database.AccountConcurrencyRule `json:"concurrency_rules" binding:"required"`
		DefaultTimezone       *string                            `json:"default_timezone" binding:"required"`
		UseFixedProxyTimezone *bool                              `json:"use_fixed_proxy_timezone" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "账号默认值请求不完整或格式无效")
		return
	}
	cfg := database.AccountDefaultsConfig{
		ConcurrencyRules:      *req.ConcurrencyRules,
		DefaultTimezone:       strings.TrimSpace(*req.DefaultTimezone),
		UseFixedProxyTimezone: *req.UseFixedProxyTimezone,
	}
	if err := normalizeAccountDefaults(&cfg); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	h.settingsUpdateMu.Lock()
	defer h.settingsUpdateMu.Unlock()
	if err := h.db.SaveAccountDefaultsConfig(c.Request.Context(), cfg); err != nil {
		writeError(c, http.StatusInternalServerError, "保存账号默认值失败")
		return
	}
	h.store.SetAccountTimezoneDefaults(cfg.DefaultTimezone, cfg.UseFixedProxyTimezone)
	h.store.SetAccountConcurrencyDefaults(cfg.ConcurrencyRules)
	c.JSON(http.StatusOK, h.accountDefaultsResponse(cfg))
}

func normalizeAccountDefaults(cfg *database.AccountDefaultsConfig) error {
	if err := validateAccountTimezone(cfg.DefaultTimezone); err != nil {
		return err
	}
	if len(cfg.ConcurrencyRules) > 256 {
		return fmt.Errorf("最多支持 256 条账号默认并发规则")
	}
	seen := make(map[[2]string]struct{}, len(cfg.ConcurrencyRules))
	for i := range cfg.ConcurrencyRules {
		rule := &cfg.ConcurrencyRules[i]
		rule.Channel = strings.ToLower(strings.TrimSpace(rule.Channel))
		if !slices.Contains(database.AllUpstreamChannels, rule.Channel) {
			return fmt.Errorf("第 %d 条规则的平台无效", i+1)
		}
		rule.Plan = auth.NormalizeAccountConcurrencyPlan(rule.Channel, rule.Plan)
		if rule.Plan == "" || len(rule.Plan) > 100 || rule.Plan == "*" || rule.Plan == "all" || rule.Concurrency < 1 {
			return fmt.Errorf("第 %d 条规则需要指定套餐，且并发必须为正整数", i+1)
		}
		key := [2]string{rule.Channel, rule.Plan}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("第 %d 条规则的平台与套餐重复", i+1)
		}
		seen[key] = struct{}{}
	}
	return nil
}
