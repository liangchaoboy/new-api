package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// SupplyConfig contains only fields a supplier may propose for distribution.
type SupplyConfig struct {
	Name    string `json:"name" gorm:"size:128"`
	Type    int    `json:"type"`
	BaseURL string `json:"base_url" gorm:"size:512"`
	Models  string `json:"models" gorm:"type:text"`
}

const (
	SupplyTargetDirect    = "direct"
	SupplyAwaitDelivery   = "await_delivery"
	SupplyDelivered       = "delivered"
	SupplyAwaitRevocation = "await_revocation"
)

type SupplyResource struct {
	DirectState  string `json:"direct_state,omitempty" gorm:"size:32"`
	ID           string `json:"id" gorm:"primaryKey;size:32"`
	SupplierID   int    `json:"supplier_id" gorm:"index"`
	SupplyConfig `gorm:"embedded"`
	Secret       string            `json:"-" gorm:"type:text"`
	Target       string            `json:"target" gorm:"size:96"`
	UserID       int               `json:"user_id"`
	Effective    *SupplyAssignment `json:"effective,omitempty" gorm:"-"`
	ActiveID     string            `json:"active_id" gorm:"size:32"`
	Revision     int64             `json:"revision"`
	Applied      int64             `json:"applied"`
	Archived     bool              `json:"archived"`
	LastError    string            `json:"last_error" gorm:"size:256"`
}

type SupplyAssignment struct {
	KeyRef        *string `json:"-" gorm:"size:64;uniqueIndex"`
	ProviderQuota *int64  `json:"provider_quota,omitempty"`
	QuotaAt       int64   `json:"quota_at,omitempty"`
	ID            string  `json:"id" gorm:"primaryKey;size:32"`
	ResourceID    string  `json:"resource_id" gorm:"size:32;index"`
	Target        string  `json:"target" gorm:"size:96"`
	UserID        int     `json:"user_id"`
	ChannelID     int     `json:"channel_id"`
	Started       int64   `json:"started"`
	Ended         int64   `json:"ended"`
	NextDay       int64   `json:"-"`
	Retiring      bool    `json:"retiring"`
}
type SupplyEvent struct {
	ID         int64  `json:"id" gorm:"primaryKey"`
	ResourceID string `json:"resource_id" gorm:"size:32;index"`
	Actor      int    `json:"actor"`
	Action     string `json:"action" gorm:"size:64"`
	Target     string `json:"target" gorm:"size:96"`
	UserID     int    `json:"user_id"`
	Revision   int64  `json:"revision"`
	Created    int64  `json:"created"`
}
type SupplyReceipt struct {
	ID        string `json:"id" gorm:"primaryKey;size:32"`
	Issuer    int    `json:"-"`
	UserID    int    `json:"user_id"`
	Version   int64  `json:"version"`
	ChannelID int    `json:"channel_id"`
	Digest    string `json:"-" gorm:"size:64"`
	Retired   bool   `json:"retired"`
}
type SupplyCommand struct {
	Version int64        `json:"version"`
	UserID  int          `json:"user_id"`
	Retire  bool         `json:"retire"`
	Config  SupplyConfig `json:"config"`
	Key     string       `json:"key"`
}
type SupplyConsumption struct {
	ModelName    string `json:"model"`
	Records      int64  `json:"records"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}
type SupplyWindow struct {
	AssignmentID string `gorm:"primaryKey;size:32"`
	Day          int64  `gorm:"primaryKey;autoIncrement:false"`
	Content      string `gorm:"type:text"`
	Collected    int64
	Incomplete   bool
}

var ErrSupplyConflict = errors.New("Configuration has changed")
var ErrSupplyInput = errors.New("Invalid parameters")

func SupplyAudit(tx *gorm.DB, resource *SupplyResource, actor int, action, target string, user int) error {
	return tx.Create(&SupplyEvent{ResourceID: resource.ID, Actor: actor, Action: action, Target: target, UserID: user, Revision: resource.Revision, Created: common.GetTimestamp()}).Error
}

// EditSupply serializes supplier edits, administrative assignments and worker acknowledgements.
func EditSupply(id string, owner int, change func(*gorm.DB, *SupplyResource) error) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		query := lockForUpdate(tx)
		if owner > 0 {
			query = query.Where("supplier_id = ?", owner)
		}
		var r SupplyResource
		if err := query.First(&r, "id = ?", id).Error; err != nil {
			return err
		}
		if err := change(tx, &r); err != nil {
			return err
		}
		return tx.Save(&r).Error
	})
}

// ReceiveSupply commits the channel and its receipt together. A retirement also
// creates a tombstone when an earlier creation is still in flight.
func ReceiveSupply(id string, issuer int, cmd SupplyCommand) (SupplyReceipt, error) {
	var receipt SupplyReceipt
	payload, err := common.Marshal(cmd)
	if err != nil {
		return receipt, err
	}
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	changed := false
	err = DB.Session(&gorm.Session{Logger: logger.Discard}).Transaction(func(tx *gorm.DB) error {
		row := SupplyReceipt{ID: id, Issuer: issuer, UserID: cmd.UserID}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).First(&receipt, "id = ? AND issuer = ?", id, issuer).Error; err != nil {
			return err
		}
		if receipt.UserID != cmd.UserID || cmd.Version < receipt.Version || receipt.Retired && !cmd.Retire {
			return ErrSupplyConflict
		}
		if receipt.Version == cmd.Version {
			if receipt.Digest != digest {
				return ErrSupplyConflict
			}
			return nil
		}
		if cmd.Retire {
			if receipt.ChannelID != 0 {
				if err := tx.Where("channel_id = ?", receipt.ChannelID).Delete(&Ability{}).Error; err != nil {
					return err
				}
				if err := tx.Where("id = ? AND assigned_user_id = ?", receipt.ChannelID, receipt.UserID).Model(&Channel{}).Updates(map[string]any{"status": common.ChannelStatusManuallyDisabled, "key": "", "models": ""}).Error; err != nil {
					return err
				}
			}
			receipt.Retired = true
		} else {
			var user User
			if err := tx.Select("id", "group", "status").First(&user, "id = ?", cmd.UserID).Error; err != nil {
				return err
			}
			if user.Status != common.UserStatusEnabled {
				return ErrSupplyInput
			}
			channel := Channel{SupplyAssignmentID: &receipt.ID, AssignedUserID: &cmd.UserID, Name: cmd.Config.Name, Type: cmd.Config.Type, BaseURL: &cmd.Config.BaseURL, Group: user.Group, Models: cmd.Config.Models, Key: cmd.Key, Status: common.ChannelStatusEnabled, CreatedTime: common.GetTimestamp()}
			if receipt.ChannelID == 0 {
				if err := tx.Create(&channel).Error; err != nil {
					return err
				}
				if err := channel.AddAbilities(tx); err != nil {
					return err
				}
				receipt.ChannelID = channel.Id
			} else {
				channel.Id = receipt.ChannelID
				var existing Channel
				if err := lockForUpdate(tx).First(&existing, "id = ? AND assigned_user_id = ?", channel.Id, cmd.UserID).Error; err != nil {
					return err
				}
				channel.Weight, channel.Priority, channel.Status = existing.Weight, existing.Priority, existing.Status
				if err := tx.Model(&existing).Select("name", "type", "base_url", "group", "models", "key").Updates(&channel).Error; err != nil {
					return err
				}
				if err := channel.UpdateAbilities(tx); err != nil {
					return err
				}
			}
		}
		receipt.Version = cmd.Version
		receipt.Digest = digest
		changed = true
		return tx.Save(&receipt).Error
	})
	if err == nil && changed {
		BumpSyncTopic(SyncTopicChannels)
		InitChannelCache()
	}
	return receipt, err
}

func SupplyDayUsage(receipt SupplyReceipt, day int64) ([]SupplyConsumption, error) {
	if !common.LogConsumeEnabled {
		return nil, fmt.Errorf("Consumption logging is disabled")
	}
	rows := []SupplyConsumption{}
	if receipt.ChannelID == 0 {
		return rows, nil
	}
	err := LOG_DB.Model(&Log{}).Select("model_name, COUNT(*) AS records, SUM(prompt_tokens) AS input_tokens, SUM(completion_tokens) AS output_tokens").
		Where("channel_id = ? AND user_id = ? AND type = ? AND created_at >= ? AND created_at < ?", receipt.ChannelID, receipt.UserID, LogTypeConsume, day, day+86400).
		Group("model_name").Order("model_name").Scan(&rows).Error
	return rows, err
}

// NextSupplyUsageDay locates late settlements without requesting every empty
// day after a channel's retirement. The remote channel ID is never reused.
func NextSupplyUsageDay(r SupplyReceipt, from int64) (int64, error) {
	if !common.LogConsumeEnabled {
		return 0, errors.New("Consumption logging is disabled")
	}
	var at int64
	err := LOG_DB.Model(&Log{}).Select("COALESCE(MIN(created_at),0)").Where("channel_id = ? AND user_id = ? AND type = ? AND created_at >= ?", r.ChannelID, r.UserID, LogTypeConsume, from).Scan(&at).Error
	return at / 86400 * 86400, err
}

func (cfg SupplyConfig) Valid() bool {
	if strings.TrimSpace(cfg.Name) == "" || len(cfg.Name) > 128 || len(cfg.BaseURL) > 512 || cfg.Models == "" || len(cfg.Models) > 8192 {
		return false
	}
	for name := range strings.SplitSeq(cfg.Models, ",") {
		if strings.TrimSpace(name) != name || len(name) == 0 || len(name) > 255 {
			return false
		}
	}
	return true
}

type SupplyTarget struct {
	Mode       string  `json:"mode,omitempty" gorm:"size:16"`
	AddressRef *string `json:"-" gorm:"size:64;uniqueIndex"`
	Issuer     int     `json:"-"`
	ID         string  `json:"id" gorm:"primaryKey;size:32"`
	Name       string  `json:"name" gorm:"size:128"`
	URL        string  `json:"url" gorm:"size:512"`
	Secret     string  `json:"-" gorm:"type:text"`
}

func StoreSupplyWindow(w *SupplyWindow) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		var assignment SupplyAssignment
		if err := lockForUpdate(tx).Select("id").First(&assignment, "id = ?", w.AssignmentID).Error; err != nil {
			return err
		}
		var old SupplyWindow
		err := tx.First(&old, "assignment_id = ? AND day = ?", w.AssignmentID, w.Day).Error
		if err == nil && old.Collected > w.Collected {
			*w = old
			return nil
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil {
			// Consume rows only accumulate. Source cleanup, partial imports and
			// out-of-order replies must not erase a previously observed total.
			var previous, current []SupplyConsumption
			if err := common.UnmarshalJsonStr(old.Content, &previous); err != nil {
				return err
			}
			if err := common.UnmarshalJsonStr(w.Content, &current); err != nil {
				return err
			}
			byModel := make(map[string]SupplyConsumption, len(current))
			for _, r := range current {
				byModel[r.ModelName] = r
			}
			for _, r := range previous {
				n := byModel[r.ModelName]
				if n.Records < r.Records || n.InputTokens < r.InputTokens || n.OutputTokens < r.OutputTokens {
					*w = old
					w.Incomplete = true
					return tx.Model(w).Update("incomplete", true).Error
				}
			}
		}
		return tx.Save(w).Error
	})
}
