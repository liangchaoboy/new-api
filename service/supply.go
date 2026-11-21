package service

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

func supplyCipher() (cipher.AEAD, error) {
	if os.Getenv("CRYPTO_SECRET") == "" && os.Getenv("SESSION_SECRET") == "" {
		return nil, errors.New("Configure a persistent encryption secret")
	}
	key := sha256.Sum256([]byte("new-api/supply/v1/" + common.CryptoSecret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}
func SealSupplySecret(id, value string) (string, error) {
	aead, err := supplyCipher()
	if err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(aead.Seal(nil, nil, []byte(value), []byte(id))), nil
}
func openSupplySecret(id, secret string) (string, error) {
	aead, err := supplyCipher()
	if err != nil {
		return "", err
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(secret)
	if err != nil {
		return "", err
	}
	data, err := aead.Open(nil, nil, ciphertext, []byte(id))
	return string(data), err
}

// SupplyURL accepts HTTPS and local loopback fixtures. Credentials and URL
// query parameters belong in neither target registrations nor audit records.
func SupplyURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback()
}
func ValidSupplyConfig(cfg model.SupplyConfig, receiver bool) bool {
	if !cfg.Valid() || !slices.Contains([]int{constant.ChannelTypeOpenAI, constant.ChannelTypeAnthropic, constant.ChannelTypeGemini, constant.ChannelTypeDeepSeek, constant.ChannelTypeNewAPI, constant.ChannelTypeOpenRouter}, cfg.Type) {
		return false
	}
	if cfg.BaseURL == "" {
		return constant.GetChannelBaseURL(cfg.Type) != ""
	}
	if !SupplyURL(cfg.BaseURL) {
		return false
	}
	if !receiver || cfg.BaseURL == constant.GetChannelBaseURL(cfg.Type) {
		return true
	}
	for allowed := range strings.SplitSeq(os.Getenv("SUPPLY_UPSTREAM_URLS"), ",") {
		if cfg.BaseURL == strings.TrimSpace(allowed) {
			return true
		}
	}
	return false
}

func callSupplyTarget[T any](ctx context.Context, targetID, method, path string, body any) (T, error) {
	var zero T
	var target model.SupplyTarget
	if err := model.DB.WithContext(ctx).First(&target, "id = ?", targetID).Error; err != nil {
		return zero, err
	}
	key, err := openSupplySecret("target/"+target.ID, target.Secret)
	if err != nil {
		return zero, err
	}
	var buf []byte
	if body != nil {
		buf, err = common.Marshal(body)
		if err != nil {
			return zero, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(target.URL, "/")+"/api/supply-receiver/"+path, bytes.NewReader(buf))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return zero, errors.New("Target unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return zero, errors.New("Target rejected the request")
	}
	var envelope struct {
		Success bool
		Data    *T
	}
	if err = common.DecodeJson(io.LimitReader(resp.Body, 2<<20), &envelope); err != nil || !envelope.Success || envelope.Data == nil {
		return zero, errors.New("Target rejected the request")
	}
	return *envelope.Data, nil
}

func ReconcileSupply(ctx context.Context, id string) error {
	var assignment model.SupplyAssignment
	var command model.SupplyCommand
	err := model.EditSupply(id, 0, func(tx *gorm.DB, r *model.SupplyResource) error {
		if r.Applied == r.Revision {
			return nil
		}
		if r.ActiveID == "" {
			if r.Target == "" {
				r.Applied = r.Revision
				r.LastError = ""
				return nil
			}
			assignment = model.SupplyAssignment{ID: common.GetUUID(), ResourceID: r.ID, Target: r.Target, UserID: r.UserID, Started: common.GetTimestamp()}
			if err := tx.Create(&assignment).Error; err != nil {
				return err
			}
			r.ActiveID = assignment.ID
		} else if err := tx.First(&assignment, "id = ?", r.ActiveID).Error; err != nil {
			return err
		}
		command = model.SupplyCommand{Version: r.Revision, UserID: assignment.UserID, Retire: assignment.Retiring || assignment.Target != r.Target || assignment.UserID != r.UserID, Config: r.SupplyConfig}
		if command.Retire {
			return tx.Model(&assignment).Update("retiring", true).Error
		}
		key, err := openSupplySecret("resource/"+id, r.Secret)
		command.Key = key
		return err
	})
	if err != nil || command.Version == 0 {
		return err
	}
	receipt, sendErr := callSupplyTarget[model.SupplyReceipt](ctx, assignment.Target, "PUT", assignment.ID, command)
	if sendErr == nil && (receipt.ID != assignment.ID || receipt.Version != command.Version || receipt.UserID != assignment.UserID || receipt.Retired != command.Retire || !receipt.Retired && receipt.ChannelID <= 0) {
		sendErr = errors.New("Target receipt mismatch")
	}
	return model.EditSupply(id, 0, func(tx *gorm.DB, r *model.SupplyResource) error {
		if r.ActiveID != assignment.ID || r.Revision != command.Version || r.Applied == r.Revision {
			return nil
		}
		if sendErr != nil {
			if r.LastError == sendErr.Error() {
				return nil
			}
			r.LastError = sendErr.Error()
			return model.SupplyAudit(tx, r, 0, "sync_failed", assignment.Target, assignment.UserID)
		}
		updates := map[string]any{"channel_id": receipt.ChannelID}
		action := "applied"
		r.LastError = ""
		if command.Retire {
			updates["ended"] = common.GetTimestamp()
			r.ActiveID = ""
			action = "revoked"
		}
		if r.ActiveID != "" || r.Target == "" {
			r.Applied = r.Revision
		}
		if err := tx.Model(&assignment).Updates(updates).Error; err != nil {
			return err
		}
		return model.SupplyAudit(tx, r, 0, action, assignment.Target, assignment.UserID)
	})
}

func CollectSupplyWindow(ctx context.Context, a model.SupplyAssignment, day int64) (model.SupplyWindow, error) {
	var cached model.SupplyWindow
	err := model.DB.First(&cached, "assignment_id = ? AND day = ?", a.ID, day).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return cached, err
	}
	now := common.GetTimestamp()
	if cached.Collected > 0 && (now-cached.Collected < 5 || day+2*86400 < now && cached.Collected >= day+2*86400) {
		return cached, nil
	}
	rows, err := callSupplyTarget[[]model.SupplyConsumption](ctx, a.Target, "GET", fmt.Sprintf("%s/usage?day=%d", a.ID, day), nil)
	if err != nil {
		return cached, err
	}
	raw, err := common.Marshal(rows)
	if err != nil {
		return cached, err
	}
	next := model.SupplyWindow{AssignmentID: a.ID, Day: day, Collected: now, Content: string(raw)}
	err = model.StoreSupplyWindow(&next)
	return next, err
}
func SyncSupplyResources() {
	for {
		var ids []string
		if err := model.DB.Model(&model.SupplyResource{}).Where("applied < revision").Pluck("id", &ids).Error; err == nil {
			for _, id := range ids {
				if err := ReconcileSupply(context.Background(), id); err != nil {
					common.SysError(common.LogText("supplier distribution failed for resource %s", id))
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
}

// SyncSupplyUsage seals completed UTC days after a one-day grace period and
// resumes from each assignment's persisted cursor after downtime.
func SyncSupplyUsage() {
	for {
		var assignments []model.SupplyAssignment
		today := common.GetTimestamp() / 86400 * 86400
		if err := model.DB.Where("channel_id > 0 AND (ended = 0 OR next_day <= ended + ?)", 86400).Find(&assignments).Error; err == nil {
			for _, a := range assignments {
				start := max(a.Started/86400*86400, a.NextDay)
				for day := start; day <= today; day += 86400 {
					if _, err := CollectSupplyWindow(context.Background(), a, day); err != nil {
						break
					}
					if day+2*86400 <= common.GetTimestamp() {
						if err := model.DB.Model(&a).Where("next_day < ?", day+86400).Update("next_day", day+86400).Error; err != nil {
							break
						}
					}
				}
			}
		}
		time.Sleep(time.Minute)
	}
}
