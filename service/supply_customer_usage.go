package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/model"
)

var errCustomerUsage = errors.New("Customer channel usage unavailable")

// The supplier console's Usage Export uses channel_daily with UTC seconds,
// source=log and a timezone offset in seconds. It groups by channel, not model.
type customerDailyRow struct {
	Day       *int64 `json:"day"`
	ChannelID *int   `json:"channel_id"`
	Count     *int64 `json:"count"`
	Input     *int64 `json:"prompt_tokens"`
	Output    *int64 `json:"completion_tokens"`
	Quota     *int64 `json:"quota"`
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
				rows = append(rows, model.SupplyConsumption{Records: *r.Count, InputTokens: *r.Input, OutputTokens: *r.Output, Quota: r.Quota})
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
