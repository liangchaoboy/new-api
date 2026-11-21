package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

var errCustomerUnconfirmed = errors.New("Waiting for customer channel confirmation")
var errCustomerOwnership = errors.New("Customer channel identity changed")

// The name contains a stable ownership marker and write revision. It is also
// searchable on customer versions that do not return IDs from channel creation.
type customerChannel struct {
	Status  int    `json:"status"`
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Tag     string `json:"tag"`
	Type    int    `json:"type"`
	Models  string `json:"models"`
	Group   string `json:"group"`
	BaseURL string `json:"base_url"`
}

func (c customerChannel) configHash() string {
	fields := []string{c.Name, c.Tag, strconv.Itoa(c.Type), c.Group, c.Models, c.BaseURL}
	encoded, _ := common.Marshal(fields)
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}
func customerMarker(id string) string { return "supply-" + id }
func customerChannelName(id string, revision int64, name string) string {
	return fmt.Sprintf("%s.v%d %s", customerMarker(id), revision, name)
}
func (s *supplyLoginSession) findChannel(ctx context.Context, a model.SupplyAssignment) (*customerChannel, int64, error) {
	marker := customerMarker(a.ID)
	keyword := marker
	if a.ChannelID > 0 {
		keyword = strconv.Itoa(a.ChannelID)
	}
	// A numeric keyword may also match unrelated names. Check exact IDs and
	// tags, and require a complete search before interpreting absence.
	var found *customerChannel
	var version int64
	for page := 1; ; page++ {
		var result struct {
			Items *[]customerChannel `json:"items"`
			Total *int               `json:"total"`
		}
		path := "/api/channel/search?keyword=" + url.QueryEscape(keyword) + "&p=" + strconv.Itoa(page) + "&page_size=100"
		if err := s.api(ctx, "GET", path, nil, &result); err != nil {
			return nil, 0, err
		}
		if result.Items == nil || result.Total == nil || *result.Total < 0 {
			return nil, 0, errCustomerUnconfirmed
		}
		for _, row := range *result.Items {
			if a.ChannelID > 0 && row.ID != a.ChannelID {
				continue
			}
			prefix, _, ok := strings.Cut(row.Name, " ")
			rev, valid := strings.CutPrefix(prefix, marker+".v")
			if !valid || !ok || row.Tag != marker {
				if a.ChannelID == row.ID || row.Tag == marker {
					return nil, 0, errCustomerOwnership
				}
				continue
			}
			v, err := strconv.ParseInt(rev, 10, 64)
			if err != nil || v <= 0 || row.ID <= 0 || found != nil {
				return nil, 0, errCustomerOwnership
			}
			copy := row
			found = &copy
			version = v
		}
		if (page-1)*100+len(*result.Items) >= *result.Total {
			break
		}
		if len(*result.Items) != 100 {
			return nil, 0, errCustomerUnconfirmed
		}
	}
	return found, version, nil
}

func reconcileCustomerChannel(ctx context.Context, id string, a model.SupplyAssignment, cmd model.SupplyCommand) error {
	var target model.SupplyTarget
	if err := model.DB.WithContext(ctx).First(&target, "id = ?", a.Target).Error; err != nil {
		return err
	}
	err := withCustomerSession(ctx, target, func(session *supplyLoginSession) error {
		observed := a
		remote, version, err := session.findChannel(ctx, a)
		if err != nil {
			return err
		}
		action := ""
		group := ""
		err = model.EditSupply(id, 0, func(tx *gorm.DB, r *model.SupplyResource) error {
			if r.ActiveID != a.ID {
				return nil
			}
			if err := tx.First(&a, "id = ?", a.ID).Error; err != nil {
				return err
			}
			if a.ChannelID != observed.ChannelID || a.Intent != observed.Intent || a.IntentRevision != observed.IntentRevision {
				return nil
			}
			if a.Intent != "" {
				if a.Intent == "retire" && remote != nil && version == a.IntentRevision && remote.Status == common.ChannelStatusManuallyDisabled {
					return finishCustomerRemoval(tx, r, &a)
				}
				if a.Intent != "retire" && remote != nil && version == a.IntentRevision && remote.configHash() == a.IntentHash {
					return finishCustomerWrite(tx, r, &a, remote.ID)
				}
				if a.Intent == "retire" && remote != nil {
					action = "retire"
					// All operations for this old assignment now converge on a
					// disabled record. Retrying retirement cannot activate it.
					cmd.Version = a.IntentRevision
					return nil
				}
				return errCustomerUnconfirmed
			}
			if r.Revision != cmd.Version || r.Applied == r.Revision {
				return nil
			}
			if cmd.Retire {
				if remote == nil && a.ChannelID != 0 {
					return errCustomerUnconfirmed
				}
				if remote == nil {
					return finishCustomerRemoval(tx, r, &a)
				}
				action = "retire"
			} else if remote == nil {
				if a.ChannelID != 0 {
					return errCustomerOwnership
				}
				action = "create"
			} else {
				if a.ChannelID != remote.ID {
					return errCustomerOwnership
				}
				action = "update"
			}
			group = r.CustomerGroup
			a.IntentHash = customerChannel{Name: customerChannelName(a.ID, cmd.Version, cmd.Config.Name), Tag: customerMarker(a.ID), Type: cmd.Config.Type, Models: cmd.Config.Models, BaseURL: cmd.Config.BaseURL, Group: group}.configHash()
			a.Intent = action
			a.IntentRevision = cmd.Version
			return tx.Model(&a).Updates(map[string]any{"intent": action, "intent_revision": cmd.Version, "intent_hash": a.IntentHash}).Error
		})
		if err != nil || action == "" {
			return err
		}
		row := map[string]any{"name": customerChannelName(a.ID, cmd.Version, cmd.Config.Name), "type": cmd.Config.Type, "tag": customerMarker(a.ID), "group": group, "models": cmd.Config.Models, "base_url": cmd.Config.BaseURL, "key": cmd.Key}
		switch action {
		case "create":
			err = session.api(ctx, "POST", "/api/channel/", map[string]any{"mode": "single", "channel": row}, nil)
		case "update":
			row["id"] = remote.ID
			err = session.api(ctx, "PUT", "/api/channel/", row, nil)
		case "retire":
			// Scrub the key and retain the remote ID to keep old logs attributable.
			if version != cmd.Version {
				row["id"] = remote.ID
				row["name"] = customerChannelName(a.ID, cmd.Version, "retired")
				row["key"] = common.GetUUID()
				row["type"] = remote.Type
				row["group"] = remote.Group
				row["models"] = remote.Models
				row["base_url"] = remote.BaseURL
				err = session.api(ctx, "PUT", "/api/channel/", row, nil)
			}
			if err == nil {
				err = session.api(ctx, "POST", "/api/channel/"+strconv.Itoa(remote.ID)+"/status", map[string]int{"status": common.ChannelStatusManuallyDisabled}, nil)
				if errors.Is(err, errCustomerEndpointMissing) {
					err = session.api(ctx, "PUT", "/api/channel/", map[string]int{"id": remote.ID, "status": common.ChannelStatusManuallyDisabled}, nil)
				}
			}
		}
		if err != nil {
			// An explicit rejection is final. A transport/decoding failure is
			// ambiguous: leave the intent in place; future passes only read back.
			if errors.Is(err, ErrCustomerDenied) || errors.Is(err, errCustomerSessionExpired) || errors.Is(err, errCustomerRejected) {
				if clearErr := model.DB.Model(&a).Where("intent = ? AND intent_revision = ?", action, cmd.Version).Updates(map[string]any{"intent": "", "intent_revision": 0}).Error; clearErr != nil {
					return clearErr
				}
			}
			return err
		}
		remote, version, err = session.findChannel(ctx, a)
		if err != nil {
			return err
		}
		return model.EditSupply(id, 0, func(tx *gorm.DB, r *model.SupplyResource) error {
			if r.ActiveID != a.ID {
				return nil
			}
			if err := tx.First(&a, "id = ?", a.ID).Error; err != nil {
				return err
			}
			if a.Intent != action || a.IntentRevision != cmd.Version {
				return nil
			}
			if action == "retire" {
				if remote == nil || version != cmd.Version || remote.Status != common.ChannelStatusManuallyDisabled {
					return errCustomerUnconfirmed
				}
				return finishCustomerRemoval(tx, r, &a)
			}
			if remote == nil || version != cmd.Version || remote.configHash() != a.IntentHash {
				return errCustomerUnconfirmed
			}
			return finishCustomerWrite(tx, r, &a, remote.ID)
		})
	})
	if err == nil {
		return nil
	}
	// Store only local error codes. Downstream messages can contain credentials.
	msg := "Customer channel sync failed"
	for _, safe := range []error{errCustomerUnconfirmed, errCustomerOwnership, ErrCustomerDenied, errCustomerSessionExpired} {
		if errors.Is(err, safe) {
			msg = safe.Error()
			break
		}
	}
	return model.EditSupply(id, 0, func(tx *gorm.DB, r *model.SupplyResource) error {
		if r.ActiveID != a.ID || r.Revision != cmd.Version || r.Applied == r.Revision || r.LastError == msg {
			return nil
		}
		r.LastError = msg
		return model.SupplyAudit(tx, r, 0, "sync_failed", a.Target, 0)
	})
}
func finishCustomerWrite(tx *gorm.DB, r *model.SupplyResource, a *model.SupplyAssignment, channelID int) error {
	if channelID <= 0 || a.ChannelID != 0 && a.ChannelID != channelID {
		return errCustomerOwnership
	}
	rev := a.IntentRevision
	if err := tx.Model(a).Updates(map[string]any{"channel_id": channelID, "intent": "", "intent_revision": 0}).Error; err != nil {
		return err
	}
	if r.Revision == rev {
		r.Applied = r.Revision
	}
	r.LastError = ""
	return model.SupplyAudit(tx, r, 0, "customer_channel_saved", a.Target, 0)
}
func finishCustomerRemoval(tx *gorm.DB, r *model.SupplyResource, a *model.SupplyAssignment) error {
	if err := tx.Model(a).Updates(map[string]any{"ended": common.GetTimestamp(), "intent": "", "intent_revision": 0}).Error; err != nil {
		return err
	}
	r.ActiveID = ""
	r.LastError = ""
	if r.Target == "" {
		r.Applied = r.Revision
	}
	return model.SupplyAudit(tx, r, 0, "customer_channel_removed", a.Target, 0)
}
