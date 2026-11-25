package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SpendLog is one request of LiteLLM's spend logs, the fields the usage profiles read. APIKey is LiteLLM's hash of the key, never the key.
type SpendLog struct {
	APIKey           string          `json:"api_key"`
	ModelGroup       string          `json:"model_group"`
	Model            string          `json:"model"`
	PromptTokens     int             `json:"prompt_tokens"`
	CompletionTokens int             `json:"completion_tokens"`
	StartTime        string          `json:"startTime"`
	EndTime          string          `json:"endTime"`
	Status           string          `json:"status"`
	RequestTags      json.RawMessage `json:"request_tags"`
	Metadata         map[string]any  `json:"metadata"`
}

// spendPage is a page of /spend/logs/ui (and /spend/logs/v2, same shape).
type spendPage struct {
	Data       []SpendLog `json:"data"`
	Total      int        `json:"total"`
	Page       int        `json:"page"`
	TotalPages int        `json:"total_pages"`
}

// spendPageSize is the largest page LiteLLM serves.
const spendPageSize = 100

// MaxSpendPages bounds one read (100 per page): a range with more requests is cut, and the report says how many it counted.
var MaxSpendPages = 3000

// SpendLogs reads the spend logs of [from, to) page by page (GET /spend/logs/ui with the master key; /spend/logs/v2 where a LiteLLM has only that).
func (c *Client) SpendLogs(ctx context.Context, from, to time.Time) ([]SpendLog, error) {
	path := "/spend/logs/ui"
	var out []SpendLog
	for page := 1; page <= MaxSpendPages; page++ {
		q := url.Values{
			"start_date": {from.UTC().Format("2006-01-02 15:04:05")},
			"end_date":   {to.UTC().Format("2006-01-02 15:04:05")},
			"page":       {strconv.Itoa(page)},
			"page_size":  {strconv.Itoa(spendPageSize)},
		}
		var p spendPage
		err := c.do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, &p)
		if page == 1 && path == "/spend/logs/ui" && IsStatus(err, http.StatusNotFound) {
			path = "/spend/logs/v2"
			page = 0
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("spend logs page %d: %w", page, err)
		}
		out = append(out, p.Data...)
		if len(p.Data) < spendPageSize || (p.TotalPages > 0 && page >= p.TotalPages) {
			break
		}
	}
	return out, nil
}

// SpendTime reads LiteLLM's timestamps: RFC 3339, or without a zone (UTC), with or without fractions.
func SpendTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999", "2006-01-02 15:04:05.999999", "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// UserAgent is the user agent LiteLLM noted for the request: its request tag "User-Agent: …" (newer LiteLLM tags every request so), else the header in the metadata; "" when neither is there.
func (l SpendLog) UserAgent() string {
	var tags []string
	if len(l.RequestTags) > 0 && json.Unmarshal(l.RequestTags, &tags) == nil {
		for _, t := range tags {
			if v, ok := strings.CutPrefix(t, "User-Agent: "); ok {
				return v
			}
		}
	}
	if h, ok := l.Metadata["headers"].(map[string]any); ok {
		for k, v := range h {
			if strings.EqualFold(k, "user-agent") {
				if s, ok := v.(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

// TeamAlias is the alias of the key's team as the metadata names it.
func (l SpendLog) TeamAlias() string {
	s, _ := l.Metadata["user_api_key_team_alias"].(string)
	return s
}
