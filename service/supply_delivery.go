package service

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

var ErrDedicatedKey = errors.New("A new dedicated upstream key is required")

type SupplyDelivery struct {
	model.SupplyConfig
	Key string `json:"key"`
}

// ExportSupplyDelivery marks disclosure in the same transaction as the audit
// event. The caller checks a one-use proof bound to the resource, target and
// revision before entering this function.
func ExportSupplyDelivery(ctx SupplyDeliveryContext, actor int) (SupplyDelivery, error) {
	var out SupplyDelivery
	err := model.EditSupply(ctx.ResourceID, 0, func(tx *gorm.DB, r *model.SupplyResource) error {
		if r.Archived || r.Target != ctx.Target || r.Revision != ctx.Revision || r.UserID != 0 || (r.DirectState != model.SupplyAwaitDelivery && r.DirectState != model.SupplyDelivered) {
			return model.ErrSupplyConflict
		}
		var a model.SupplyAssignment
		if err := tx.First(&a, "id = ? AND resource_id = ? AND target = ? AND user_id = 0 AND ended = 0 AND retiring = ?", r.ActiveID, r.ID, r.Target, false).Error; err != nil {
			return err
		}
		if r.Secret == "" {
			return ErrDedicatedKey
		}
		key, err := openSupplySecret("resource/"+r.ID, r.Secret)
		if err != nil {
			return err
		}
		ref := common.GenerateHMAC(key)
		if a.KeyRef != nil && *a.KeyRef != ref {
			return ErrDedicatedKey
		}
		if a.KeyRef == nil {
			var count int64
			if err := tx.Model(&model.SupplyAssignment{}).Where("key_ref = ?", ref).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return ErrDedicatedKey
			}
			if err := tx.Model(&a).Update("key_ref", ref).Error; err != nil {
				return err
			}
		}
		r.Applied = r.Revision
		r.DirectState = model.SupplyDelivered
		r.LastError = ""
		out = SupplyDelivery{r.SupplyConfig, key}
		if out.BaseURL == "" {
			out.BaseURL = constant.GetChannelBaseURL(out.Type)
		}
		return model.SupplyAudit(tx, r, actor, "key_exported", r.Target, 0)
	})
	if err != nil {
		return SupplyDelivery{}, err
	}
	return out, nil
}

func ConfirmSupplyRevocation(id, assignmentID string, revision int64, actor, owner int) error {
	return model.EditSupply(id, owner, func(tx *gorm.DB, r *model.SupplyResource) error {
		if r.DirectState != model.SupplyAwaitRevocation || r.Revision != revision || assignmentID == "" || r.ActiveID != assignmentID {
			return model.ErrSupplyConflict
		}
		var a model.SupplyAssignment
		if err := tx.First(&a, "id = ? AND user_id = 0 AND ended = 0 AND retiring = ? AND key_ref IS NOT NULL", assignmentID, true).Error; err != nil {
			return err
		}
		if err := tx.Model(&a).Update("ended", common.GetTimestamp()).Error; err != nil {
			return err
		}
		r.ActiveID = ""
		r.DirectState = ""
		r.LastError = ""
		r.Secret = ""
		if r.Target == "" {
			r.Applied = r.Revision
		} else {
			r.LastError = ErrDedicatedKey.Error()
		}
		return model.SupplyAudit(tx, r, actor, "revocation_confirmed", a.Target, 0)
	})
}

// RefreshDirectProviderQuota reads only the dedicated token's cumulative quota on a
// New API upstream. It never queries the downstream customer's personal logs.
func RefreshDirectProviderQuota(ctx context.Context, a *model.SupplyAssignment) error {
	now := common.GetTimestamp()
	if a.ProviderQuota != nil && now-a.QuotaAt < 30 {
		return nil
	}
	var r model.SupplyResource
	if err := model.DB.WithContext(ctx).First(&r, "id = ? AND active_id = ?", a.ResourceID, a.ID).Error; err != nil {
		return err
	}
	quota, err := directProviderQuota(ctx, r, *a)
	if err != nil {
		return err
	}
	change := model.DB.WithContext(ctx).Model(a).Where("ended = 0 AND (provider_quota IS NULL OR provider_quota <= ?) AND (quota_at IS NULL OR quota_at <= ?)", quota, now).Updates(map[string]any{"provider_quota": quota, "quota_at": now})
	if change.Error != nil {
		return change.Error
	}
	if change.RowsAffected != 1 {
		return model.ErrSupplyConflict
	}
	a.ProviderQuota = &quota
	a.QuotaAt = now
	return nil
}

func SyncDirectProviderUsage() {
	for {
		var assignments []model.SupplyAssignment
		if err := model.DB.Where("ended = 0 AND key_ref IS NOT NULL").Find(&assignments).Error; err == nil {
			for i := range assignments {
				_ = RefreshDirectProviderQuota(context.Background(), &assignments[i])
			}
		}
		time.Sleep(5 * time.Minute)
	}
}
func directProviderQuota(ctx context.Context, r model.SupplyResource, a model.SupplyAssignment) (int64, error) {
	if r.Type != constant.ChannelTypeNewAPI || !ValidSupplyConfig(r.SupplyConfig, false) || a.KeyRef == nil || r.ActiveID != a.ID || r.Secret == "" {
		return 0, errors.New("Usage unavailable")
	}
	key, err := openSupplySecret("resource/"+r.ID, r.Secret)
	if err != nil || common.GenerateHMAC(key) != *a.KeyRef {
		return 0, errors.New("Usage unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(r.BaseURL, "/")+"/api/usage/token/", nil)
	if err != nil {
		return 0, errors.New("Usage unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	client := http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if !ValidSupplyConfig(r.SupplyConfig, true) {
		// Supplier input: restrict unlisted origins to public HTTPS, including
		// the actual DNS address selected by the dialer.
		if req.URL.Scheme != "https" {
			return 0, errors.New("Usage unavailable")
		}
		client.Transport = publicSupplyTransport
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, errors.New("Usage unavailable")
	}
	defer resp.Body.Close()
	var result struct {
		Code bool
		Data struct {
			Object string
			Used   *int64 `json:"total_used"`
		}
	}
	if resp.StatusCode != 200 || common.DecodeJson(io.LimitReader(resp.Body, 65536), &result) != nil || !result.Code || result.Data.Object != "token_usage" || result.Data.Used == nil || *result.Data.Used < 0 {
		return 0, errors.New("Usage unavailable")
	}
	return *result.Data.Used, nil
}

// This transport never forwards an upstream key across origins or delegates
// hostname resolution to an untrusted proxy.
var publicSupplyTransport = &http.Transport{DialContext: (&protectedFetchDialer{
	resolver: net.DefaultResolver, dialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	getProtection: func() (*common.SSRFProtection, bool, error) {
		return &common.SSRFProtection{ApplyIPFilterForDomain: true}, true, nil
	},
}).DialContext, IdleConnTimeout: 30 * time.Second}
