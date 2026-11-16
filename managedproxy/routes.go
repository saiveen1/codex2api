package managedproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

func routeGroupName(node Node) string { return "mp_route_" + node.Key }

func routeGroup(node Node) map[string]any {
	return map[string]any{
		"name": routeGroupName(node), "type": "select",
		"proxies": []string{"REJECT"}, "use": []string{providerName(node.SubscriptionID)},
		"filter": "^(?:REJECT|" + regexp.QuoteMeta(prefix(node.SubscriptionID)+node.Name) + ")$",
	}
}

// Provider nodes are not entries in mihomo's global outbound map. Select them
// through a dedicated group and verify the selection before publishing listeners.
func (p *coreProcess) selectNode(ctx context.Context, node Node) error {
	if !p.alive() {
		return errors.New("managed proxy: owned core is not running")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	target := prefix(node.SubscriptionID) + node.Name
	body, _ := json.Marshal(map[string]string{"name": target})
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	path := "/proxies/" + url.PathEscape(routeGroupName(node))
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://127.0.0.1:"+strconv.Itoa(p.port)+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("managed proxy: invalid node selection request")
	}
	request.Header.Set("Authorization", "Bearer "+p.secret)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return errors.New("managed proxy: node selection request failed")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
	if response.StatusCode != 200 && response.StatusCode != 204 {
		return errors.New("managed proxy: native core rejected node selection")
	}
	var selected struct {
		Now string `json:"now"`
	}
	if err := p.request(ctx, path, &selected); err != nil {
		return err
	}
	if selected.Now != target {
		return errors.New("managed proxy: native node selection was not confirmed")
	}
	return nil
}

func (p *coreProcess) selectNodes(ctx context.Context, nodes []Node) error {
	for _, node := range nodes {
		if node.Active {
			if err := p.selectNode(ctx, node); err != nil {
				return err
			}
		}
	}
	return nil
}
