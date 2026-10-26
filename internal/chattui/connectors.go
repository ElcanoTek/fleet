package chattui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// catalogRow is the part of GET /mcp-servers a new conversation's connector
// selection needs. It is the same catalog the web chat seeds its Tools picker
// from: `enabled` already folds in the user's connector preferences and the
// bundle's enabled_by_default (bundledPrefFor), so the CLI does not re-derive
// either.
type catalogRow struct {
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	AlwaysOn bool   `json:"always_on"`
	Status   string `json:"status"`
}

// newConversationConnectors is the enabled_optional a NEW conversation opens
// with: the --connector names when given (they replace the defaults), else
// what a new web chat starts with (enabled and not always-on, as the web's
// enabledOptionalMcpServerNames). A catalog that cannot be read is an error:
// silently opening a conversation with no connectors is what this fixes.
func (c *Client) newConversationConnectors(ctx context.Context) ([]string, error) {
	rows, err := c.connectorCatalog(ctx)
	if err != nil {
		return nil, err
	}
	if len(c.cfg.Connectors) == 0 {
		var on []string
		for _, r := range rows {
			if r.Enabled && !r.AlwaysOn {
				on = append(on, r.Name)
			}
		}
		return on, nil
	}
	choosable := map[string]bool{}
	var names []string
	for _, r := range rows {
		if !r.AlwaysOn && (r.Status == "" || r.Status == "available") {
			choosable[strings.ToLower(r.Name)] = true
			names = append(names, strings.ToLower(r.Name))
		}
	}
	slices.Sort(names)
	var picked []string
	for _, want := range c.cfg.Connectors {
		if !choosable[want] {
			return nil, fmt.Errorf("--connector %q is not an optional connector you can use here (choose from: %s)",
				want, strings.Join(names, ", "))
		}
		picked = append(picked, want)
	}
	return picked, nil
}

// connectorCatalog reads GET /mcp-servers as this user.
func (c *Client) connectorCatalog(ctx context.Context) ([]catalogRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.ServerURL+"/mcp-servers", nil)
	if err != nil {
		return nil, err
	}
	c.setAuthHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read the connector catalog: connect %s: %w", c.cfg.ServerURL, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, c.refusal(resp)
	default:
		return nil, fmt.Errorf("read the connector catalog: GET /mcp-servers answered %d", resp.StatusCode)
	}
	var body struct {
		Servers []catalogRow `json:"servers"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("read the connector catalog: %w", err)
	}
	return body.Servers, nil
}

// normalizeConnectors lower-cases, trims and de-duplicates --connector names
// (the server canonicalizes connector names to lower case).
func normalizeConnectors(in []string) []string {
	var out []string
	for _, n := range in {
		n = strings.ToLower(strings.TrimSpace(n))
		if n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}
