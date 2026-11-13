package client

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"strconv"
)

const maxPages = 10_000 // guard against a server that never stops paginating

// page is Asana's list envelope.
type page struct {
	Data     []json.RawMessage `json:"data"`
	NextPage *struct {
		Offset string `json:"offset"`
	} `json:"next_page"` // null on the last page
}

// FetchAll GETs path and follows Asana's offset pagination until the last page.
// It returns every item of every page, or an error (never a partial result).
func (c *Client) FetchAll(ctx context.Context, path string, params url.Values) ([]json.RawMessage, error) {
	all := make([]json.RawMessage, 0) // [] not null
	offset := ""
	for pageNum := 1; pageNum <= maxPages; pageNum++ {
		body, err := c.get(ctx, c.pageURL(path, params, offset))
		if err != nil {
			return nil, fmt.Errorf("fetch %s page %d: %w", path, pageNum, err)
		}
		var p page
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("decode %s page %d: %w", path, pageNum, err)
		}
		all = append(all, p.Data...)
		c.log.Debug("fetched page", "path", path, "page", pageNum, "items", len(p.Data))

		if p.NextPage == nil || p.NextPage.Offset == "" {
			return all, nil
		}
		if p.NextPage.Offset == offset {
			return nil, fmt.Errorf("fetch %s: server repeated offset on page %d", path, pageNum)
		}
		offset = p.NextPage.Offset
	}
	return nil, fmt.Errorf("fetch %s: more than %d pages", path, maxPages)
}

func (c *Client) pageURL(path string, params url.Values, offset string) string {
	u := c.baseURL.JoinPath(path)
	q := maps.Clone(params)
	if q == nil {
		q = url.Values{}
	}
	q.Set("limit", strconv.Itoa(c.pageSize))
	if offset != "" {
		q.Set("offset", offset)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// validator is implemented by model types that can check required fields.
type validator interface{ Validate() error }

// FetchAllAs fetches every page and decodes each item into T. Items that fail
// Validate are skipped and counted; if ALL items are invalid it returns an
// error, because that usually means the API's schema changed.
// (A generic function, because Go methods can't have type parameters.)
func FetchAllAs[T any](ctx context.Context, c *Client, path string, params url.Values) ([]T, error) {
	raw, err := c.FetchAll(ctx, path, params)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(raw))
	invalid := 0
	for i, r := range raw {
		var item T
		if err := json.Unmarshal(r, &item); err != nil {
			return nil, fmt.Errorf("decode %s item %d: %w", path, i, err)
		}
		if v, ok := any(item).(validator); ok {
			if err := v.Validate(); err != nil {
				invalid++
				continue
			}
		}
		out = append(out, item)
	}
	if invalid > 0 {
		if len(out) == 0 {
			return nil, fmt.Errorf("fetch %s: all %d items are invalid", path, invalid)
		}
		c.log.Warn("skipped invalid items", "path", path, "invalid", invalid, "valid", len(out))
	}
	return out, nil
}
