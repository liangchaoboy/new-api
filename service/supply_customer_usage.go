package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

var errCustomerUsage = errors.New("Customer channel usage unavailable")

// The supplier console's Usage Export uses channel_daily with UTC seconds,
// source=log and a timezone offset in seconds. It groups by channel, not model.
type customerDailyRow struct {
	Day        *int64 `json:"day"`
	ChannelID  *int   `json:"channel_id"`
	Count      *int64 `json:"count"`
	Input      *int64 `json:"prompt_tokens"`
	Output     *int64 `json:"completion_tokens"`
	Quota      *int64 `json:"quota"`
	CacheRead  *int64 `json:"cache_read_tokens"`
	CacheWrite *int64 `json:"cache_write_tokens"`
}

func customerDailyPath(day int64) string {
	return fmt.Sprintf("/api/data/channel_daily?start_timestamp=%d&end_timestamp=%d&tz_offset=0&source=log", day, day+86399)
}

func customerDayUsage(ctx context.Context, a model.SupplyAssignment, day int64) ([]model.SupplyConsumption, error) {
	var t model.SupplyTarget
	if err := model.DB.WithContext(ctx).First(&t, "id = ?", a.Target).Error; err != nil {
		return nil, err
	}
	if a.ChannelID <= 0 || t.Mode != model.SupplyTargetAccount {
		return nil, errCustomerUsage
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows := []model.SupplyConsumption{}
	err := withCustomerSession(ctx, t, func(s *supplyLoginSession) error {
		if t.UsageAPI == "channel_daily" {
			var data *[]customerDailyRow
			if err := s.api(ctx, "GET", customerDailyPath(day), nil, &data); err != nil {
				return err
			}
			if data == nil {
				return errCustomerUsage
			}
			for _, r := range *data {
				if r.ChannelID == nil {
					return errCustomerUsage
				}
				if *r.ChannelID != a.ChannelID {
					continue
				}
				if r.Day == nil || *r.Day != day || r.Count == nil || r.Input == nil || r.Output == nil || r.Quota == nil || *r.Quota < 0 || *r.Count < 0 || *r.Input < 0 || *r.Output < 0 || len(rows) > 0 {
					return errCustomerUsage
				}
				rows = append(rows, model.SupplyConsumption{
					Records: *r.Count, InputTokens: *r.Input, OutputTokens: *r.Output, Quota: r.Quota,
					CacheReadTokens: nonNegativeCacheTokens(r.CacheRead), CacheWriteTokens: nonNegativeCacheTokens(r.CacheWrite),
				})
			}
			return nil
		}
		if t.UsageAPI != "channel_logs" {
			return errCustomerUsage
		}
		byModel := map[string]*model.SupplyConsumption{}
		// Freeze the upper bound before paginating. Validate channel IDs and
		// totals: a server ignoring filters must never leak another supplier's usage.
		end := min(day+86399, time.Now().Unix()-1)
		if end < day {
			return nil
		}
		total := -1
		for p := 1; ; p++ {
			var result struct {
				Total *int `json:"total"`
				Items *[]struct {
					Channel int   `json:"channel"`
					Created int64 `json:"created_at"`
					Type    int
					Model   string `json:"model_name"`
					Input   *int64 `json:"prompt_tokens"`
					Output  *int64 `json:"completion_tokens"`
					Quota   *int64
					Other   json.RawMessage
				} `json:"items"`
			}
			path := fmt.Sprintf("/api/log/?type=%d&channel=%d&start_timestamp=%d&end_timestamp=%d&p=%d&page_size=100", model.LogTypeConsume, a.ChannelID, day, end, p)
			if err := s.api(ctx, "GET", path, nil, &result); err != nil {
				return err
			}
			if result.Items == nil || result.Total == nil || *result.Total < 0 || total >= 0 && total != *result.Total {
				return errCustomerUsage
			}
			total = *result.Total
			if len(*result.Items) != min(100, total-(p-1)*100) {
				return errCustomerUsage
			}
			for _, r := range *result.Items {
				if r.Channel != a.ChannelID || r.Created < day || r.Created > end || r.Type != model.LogTypeConsume || strings.TrimSpace(r.Model) == "" || r.Input == nil || r.Output == nil || r.Quota == nil {
					return errCustomerUsage
				}
				item := byModel[r.Model]
				if item == nil {
					item = &model.SupplyConsumption{ModelName: r.Model, Quota: new(int64)}
					byModel[r.Model] = item
				}
				if *r.Input < 0 || *r.Output < 0 || *r.Quota < 0 || *r.Input > math.MaxInt64-item.InputTokens || *r.Output > math.MaxInt64-item.OutputTokens || *r.Quota > math.MaxInt64-*item.Quota {
					return errCustomerUsage
				}
				cacheRead, cacheWrite := customerLogCacheTokens(r.Other)
				item.CacheReadTokens = accumulateCustomerCacheTokens(item.CacheReadTokens, cacheRead, item.Records)
				item.CacheWriteTokens = accumulateCustomerCacheTokens(item.CacheWriteTokens, cacheWrite, item.Records)
				item.Records++
				item.InputTokens += *r.Input
				item.OutputTokens += *r.Output
				*item.Quota += *r.Quota
			}
			if p*100 >= total {
				break
			}
		}
		for _, item := range byModel {
			rows = append(rows, *item)
		}
		slices.SortFunc(rows, func(a, b model.SupplyConsumption) int { return strings.Compare(a.ModelName, b.ModelName) })
		return nil
	})
	return rows, err
}

// Daily exports use dedicated fields. Standard consume logs place them under
// other, which is historically encoded as a JSON string.
func customerLogCacheTokens(raw json.RawMessage) (read, write *int64) {
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[0] == '"' {
		var encoded string
		if common.Unmarshal(raw, &encoded) != nil {
			return nil, nil
		}
		raw = json.RawMessage(encoded)
	}
	var fields struct {
		Read       *int64 `json:"cache_tokens"`
		Write      *int64 `json:"cache_creation_tokens"`
		WriteTotal *int64 `json:"cache_write_tokens"`
		Write5m    *int64 `json:"cache_creation_tokens_5m"`
		Write1h    *int64 `json:"cache_creation_tokens_1h"`
		Write12h   *int64 `json:"cache_creation_tokens_12h"`
	}
	if common.Unmarshal(raw, &fields) != nil {
		return nil, nil
	}
	read = nonNegativeCacheTokens(fields.Read)
	if total := nonNegativeCacheTokens(fields.WriteTotal); total != nil {
		return read, total
	}
	write = nonNegativeCacheTokens(fields.Write)
	var split *int64
	for _, bucket := range []*int64{fields.Write5m, fields.Write1h, fields.Write12h} {
		if bucket == nil {
			continue
		}
		if *bucket < 0 {
			return read, write
		}
		if split == nil {
			split = new(int64)
		}
		if *bucket > math.MaxInt64-*split {
			return read, write
		}
		*split += *bucket
	}
	if split != nil && (write == nil || *split > *write) {
		write = split
	}
	return read, write
}

func nonNegativeCacheTokens(value *int64) *int64 {
	if value == nil || *value < 0 {
		return nil
	}
	return value
}

// A missing field on any row makes the total unknown for that model and day.
func accumulateCustomerCacheTokens(total, value *int64, records int64) *int64 {
	if records == 0 {
		return value
	}
	if total == nil || value == nil || *value > math.MaxInt64-*total {
		return nil
	}
	*total += *value
	return total
}
