package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

// The snapshot comes exclusively from the server-resolved key, never the request.
func imageJobChannel(key *database.APIKeyRow) string {
	if key == nil {
		return database.UpstreamChannelAuto
	}
	return key.Limits.ResolveUpstreamChannel()
}

func validateImageJobChannelSnapshot(req imageGenerationJobPayload, key *database.APIKeyRow) error {
	current := imageJobChannel(key)
	if req.ChannelSnapshot == "" {
		if current == database.UpstreamChannelChatGPTWeb {
			return errors.New("legacy image job cannot be routed to ChatGPT Web")
		}
		return nil
	}
	if current != req.ChannelSnapshot {
		return errors.New("image job API key channel changed since submission")
	}
	return nil
}

func (h *Handler) validateImageJobChannel(ctx context.Context, req imageGenerationJobPayload, key *database.APIKeyRow) error {
	if err := validateImageJobChannelSnapshot(req, key); err != nil {
		return err
	}
	if key != nil && key.ID > 0 {
		current, err := h.db.GetAPIKeyByID(ctx, key.ID)
		if err != nil || current == nil {
			return errors.New("image job API key is unavailable")
		}
		return validateImageJobChannelSnapshot(req, current)
	}
	return validateImageJobChannelSnapshot(req, key)
}

func imageJobOutputContext(ctx context.Context, req imageGenerationJobPayload, id int64, index int) context.Context {
	if req.ChannelSnapshot != database.UpstreamChannelChatGPTWeb {
		return ctx
	}
	return proxy.WithChatGPTWebExecutionID(ctx, fmt.Sprintf("image-job-%d-output-%d", id, index))
}

// Preserve explicitly supplied unsupported Web options for the adapter to reject.
func addWebImageJobOptions(body map[string]any, req imageGenerationJobPayload) {
	if req.ChannelSnapshot != database.UpstreamChannelChatGPTWeb {
		return
	}
	for key, value := range map[string]json.RawMessage{"mask": req.Mask, "input_fidelity": req.InputFidelity, "moderation": req.Moderation, "output_compression": req.OutputCompression, "partial_images": req.PartialImages} {
		if len(value) > 0 {
			body[key] = value
		}
	}
}
