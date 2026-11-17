package service

import "github.com/codex2api/internal/oauthgateway/upstream"

type ItemDiagnostic struct {
	UpstreamCode string `json:"upstream_code,omitempty"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	Stage        string `json:"stage,omitempty"`
}

func failureDiagnostic(item upstream.BatchItem) *ItemDiagnostic {
	if item.Error == nil {
		return nil
	}
	var d ItemDiagnostic
	if ev := item.Error.Evidence; ev != nil {
		d.UpstreamCode, d.HTTPStatus, d.Endpoint = ev.UpstreamCode, ev.HTTPStatus, ev.Endpoint
	}
	if item.Progress != nil {
		d.Stage = item.Progress.Stage
	}
	if d == (ItemDiagnostic{}) {
		return nil
	}
	return &d
}
