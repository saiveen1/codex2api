package proxy

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const (
	modelCatalogEmptyNamespace = "model-catalog-empty-v1"
	modelCatalogEmptyTTL       = 24 * time.Hour
	modelCatalogEmptyInterval  = time.Minute
	modelCatalogSampleLimit    = 8
)

// ModelCatalogDiagnostic describes the local catalog (not inference scheduling).
type ModelCatalogDiagnostic struct {
	Reason    string               `json:"reason"`
	GroupIDs  []int64              `json:"group_ids,omitempty"`
	Channel   string               `json:"channel,omitempty"`
	Scoped    *int                 `json:"scoped,omitempty"`
	Visible   *int                 `json:"visible,omitempty"`
	Models    []string             `json:"models,omitempty"`
	Counts    map[string]int       `json:"counts,omitempty"`
	Samples   []modelCatalogSample `json:"samples,omitempty"`
	Time      string               `json:"time,omitempty"`
	RequestID string               `json:"request_id,omitempty"`
}

type modelCatalogSample struct {
	AccountID int64  `json:"account_id"`
	Reason    string `json:"reason"`
}

func effectiveCatalogGroupIDs(row *database.APIKeyRow) []int64 {
	// No-affinity groups take part only when the main group allowlist is set.
	ids := slices.DeleteFunc(slices.Clone(row.AllowedGroupIDs), func(id int64) bool { return id <= 0 })
	if len(ids) == 0 {
		return nil
	}
	for _, id := range row.Limits.NoAffinityGroupIDs {
		if id > 0 {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func (d *ModelCatalogDiagnostic) addBlock(reason string, accountID int64) {
	if d.Counts == nil {
		d.Counts = make(map[string]int)
	}
	d.Counts[reason]++
	if accountID <= 0 {
		return // Scope mismatches are counts only; do not expose those account IDs.
	}
	for _, sample := range d.Samples {
		if sample.Reason == reason {
			return // Reserve room for distinct reasons, not the first N accounts.
		}
	}
	sample := modelCatalogSample{AccountID: accountID, Reason: reason}
	if len(d.Samples) < modelCatalogSampleLimit {
		d.Samples = append(d.Samples, sample)
	} else if reason == "account_key_denied" {
		d.Samples[len(d.Samples)-1] = sample
	}
}

var modelCatalogID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// InspectModelCatalog uses a fresh DB row without syncing it back into the
// scheduler. It never retrieves the upstream Codex manifest.
func (h *Handler) InspectModelCatalog(ctx context.Context, row *database.APIKeyRow) ModelCatalogDiagnostic {
	records, diagnosis := h.buildScopedModelCatalog(ctx, row, true)
	diagnosis.Time = time.Now().UTC().Format(time.RFC3339Nano)
	for _, model := range collapseAntigravityModelChoices(scopedRecordsToModels(records)) {
		if len(diagnosis.Models) == 128 {
			break
		}
		if modelCatalogID.MatchString(model.ID) {
			diagnosis.Models = append(diagnosis.Models, model.ID)
		}
	}
	return diagnosis
}

func (h *Handler) serveScopedModelList(c *gin.Context, ctx context.Context, row *database.APIKeyRow) {
	records, diagnosis := h.buildScopedModelCatalog(ctx, row, false)
	models := collapseAntigravityModelChoices(scopedRecordsToModels(records))
	if len(models) == 0 {
		h.markEmptyModelCatalog(c, row, diagnosis)
	}
	api.SendList(c, "list", models)
}

// isModelCatalogRequest identifies read-only discovery, not inference or model actions.
func isModelCatalogRequest(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.Method != http.MethodGet {
		return false
	}
	switch c.FullPath() {
	case "/models", "/v1/models", "/backend-api/codex/models", "/v1beta/models":
		return true
	case "": // Early middleware and direct handler tests may not have a route pattern.
		switch c.Request.URL.Path {
		case "/models", "/v1/models", "/backend-api/codex/models", "/v1beta/models":
			return true
		}
	}
	return false
}

var (
	catalogEmptyLogMu sync.Mutex
	catalogEmptyLogAt = map[int64]time.Time{}
)

func allowModelCatalogEmptyWrite(keyID int64, now time.Time) bool {
	catalogEmptyLogMu.Lock()
	defer catalogEmptyLogMu.Unlock()
	if last, ok := catalogEmptyLogAt[keyID]; ok && now.Sub(last) < modelCatalogEmptyInterval {
		return false
	}
	if len(catalogEmptyLogAt) >= 4096 {
		for id, at := range catalogEmptyLogAt {
			if now.Sub(at) >= modelCatalogEmptyInterval {
				delete(catalogEmptyLogAt, id)
			}
		}
		if len(catalogEmptyLogAt) >= 4096 {
			return false
		}
	}
	catalogEmptyLogAt[keyID] = now
	return true
}

func (h *Handler) markEmptyModelCatalog(c *gin.Context, row *database.APIKeyRow, diagnosis ModelCatalogDiagnostic) {
	if row == nil {
		return // Preserve anonymous manifest behavior.
	}
	c.Header("X-Codex2API-Model-Status", "no_visible_models")
	attachUpstreamTrace(c, h.store)
	now := time.Now().UTC()
	if row.ID <= 0 {
		return
	}
	// Memory leases already throttle in-process; avoid a cache round trip on
	// every repeat poll when the lease has to be acquired over the network.
	localThrottle := h.cache == nil || h.cache.SharedAcrossInstances()
	if localThrottle && !allowModelCatalogEmptyWrite(row.ID, now) {
		return
	}
	diagnosis.Time = now.Format(time.RFC3339Nano)
	if trace := upstreamTraceFromContext(c.Request.Context()); trace != nil {
		diagnosis.RequestID = trace.requestID
	}
	diagnosis.Models = nil
	if len(diagnosis.GroupIDs) > 64 {
		diagnosis.GroupIDs = diagnosis.GroupIDs[:64]
	}
	if h.cache != nil {
		key := strconv.FormatInt(row.ID, 10)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 100*time.Millisecond)
		defer cancel()
		acquired, err := h.cache.AcquireLease(ctx, modelCatalogEmptyNamespace, key, diagnosis.RequestID, modelCatalogEmptyInterval)
		if err == nil {
			if !acquired {
				return
			}
			if payload, err := json.Marshal(diagnosis); err == nil {
				_ = h.cache.SetRuntime(ctx, modelCatalogEmptyNamespace, key, payload, modelCatalogEmptyTTL)
			}
		} else if !localThrottle && !allowModelCatalogEmptyWrite(row.ID, now) {
			return
		}
	}
	writeDiagnostic(struct {
		Event string `json:"event"`
		KeyID int64  `json:"api_key_id"`
		ModelCatalogDiagnostic
	}{"model_catalog_empty", row.ID, diagnosis})
	log.Printf("model catalog empty for api_key_id=%d request_id=%s reason=%s counts=%v samples=%v", row.ID, diagnosis.RequestID, diagnosis.Reason, diagnosis.Counts, diagnosis.Samples)
}

// LastEmptyModelCatalog returns only the most recent real empty discovery, if retained.
func (h *Handler) LastEmptyModelCatalog(ctx context.Context, keyID int64) (*ModelCatalogDiagnostic, string) {
	if h == nil || h.cache == nil {
		return nil, "cache_unavailable"
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	raw, ok, err := h.cache.GetRuntime(ctx, modelCatalogEmptyNamespace, strconv.FormatInt(keyID, 10))
	if err != nil {
		return nil, "cache_unavailable"
	}
	if !ok || len(raw) > 16*1024 {
		return nil, "uncollected"
	}
	var diagnosis ModelCatalogDiagnostic
	if json.Unmarshal(raw, &diagnosis) != nil || diagnosis.Reason == "" || diagnosis.Reason == "ok" {
		return nil, "uncollected"
	}
	at, err := time.Parse(time.RFC3339Nano, diagnosis.Time)
	if err != nil || time.Since(at) >= modelCatalogEmptyTTL {
		return nil, "uncollected"
	}
	return &diagnosis, "recorded"
}
