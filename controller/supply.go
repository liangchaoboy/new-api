package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// A supplier may always manage its own resources. Cross-supplier operations
// additionally require the same admin capability as ordinary channels.
func supplyOwnerScope(c *gin.Context, permission authz.Permission) int {
	id := c.GetInt("id")
	if role := c.GetInt("role"); role >= common.RoleAdminUser && authz.Can(id, role, permission) {
		return 0
	}
	return id
}

// Downstream identities are only visible to administrators who can manage
// sensitive channel configuration and have permission to read channels.
func canViewSupplyTargets(c *gin.Context) bool {
	id, role := c.GetInt("id"), c.GetInt("role")
	return role >= common.RoleAdminUser && authz.Can(id, role, authz.ChannelRead) &&
		authz.Can(id, role, authz.ChannelSensitiveWrite)
}

var errDirectRevokeFirst = errors.New("Revoke the delivered key first")

var errSupplyForbidden = errors.New("insufficient privileges")

func supplyError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errDirectRevokeFirst):
		common.ApiErrorT(c, errDirectRevokeFirst.Error())
	case errors.Is(err, service.ErrCustomerDenied):
		common.ApiErrorStatus(c, http.StatusForbidden, common.NewMessage(service.ErrCustomerDenied.Error()))
	case errors.Is(err, errSupplyForbidden):
		common.ApiErrorStatus(c, http.StatusForbidden, common.NewMessage("Unauthorized, insufficient privileges"))
	case errors.Is(err, model.ErrSupplyInput):
		common.ApiErrorT(c, "Invalid parameters")
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(404, gin.H{"success": false, "message": "Not found"})
	case errors.Is(err, model.ErrSupplyConflict):
		c.JSON(409, gin.H{"success": false, "message": "Configuration has changed"})
	default:
		common.ApiErrorT(c, "Operation failed")
	}
}

func SupplyTargets(c *gin.Context) {
	if !canViewSupplyTargets(c) {
		supplyError(c, errSupplyForbidden)
		return
	}
	var targets []model.SupplyTarget
	if err := model.DB.Omit("secret").Order("name").Find(&targets).Error; err != nil {
		supplyError(c, err)
		return
	}
	common.ApiSuccess(c, targets)
}
func SaveSupplyTarget(c *gin.Context) {
	var req struct {
		Name, URL, Token, Username, Password string
		AccessMethod                         string `json:"access_method"`
	}
	if c.ShouldBindJSON(&req) != nil || len(req.Token) > 8192 || len(req.Password) > 2048 || len(req.Username) > 200 {
		supplyError(c, model.ErrSupplyInput)
		return
	}
	id := c.Param("id")
	if id == "" {
		id = common.GetUUID()
	}
	if req.Username != "" || req.Password != "" {
		if req.Token != "" {
			supplyError(c, model.ErrSupplyInput)
			return
		}
		var target model.SupplyTarget
		var err error
		if c.Param("id") != "" {
			if err := model.DB.First(&target, "id = ?", id).Error; err != nil {
				supplyError(c, err)
				return
			}
			if target.Mode != model.SupplyTargetDirect && target.Mode != model.SupplyTargetAccount {
				supplyError(c, model.ErrSupplyConflict)
				return
			}
		} else {
			target.ID = id
			target.Mode = model.SupplyTargetDirect
			target.URL, err = service.DirectSupplyAddress(req.URL)
			if err != nil {
				supplyError(c, model.ErrSupplyInput)
				return
			}
			target.Name = strings.TrimSpace(req.Name)
			if target.Name == "" {
				u, _ := url.Parse(target.URL)
				target.Name = u.Host
			}
			if len(target.Name) > 128 {
				supplyError(c, model.ErrSupplyInput)
				return
			}
			sum := sha256.Sum256([]byte(target.URL))
			ref := hex.EncodeToString(sum[:])
			target.AddressRef = &ref
		}
		previous := target
		if req.AccessMethod != "" && req.AccessMethod != "saved_account" {
			supplyError(c, model.ErrSupplyInput)
			return
		}
		if req.AccessMethod == "saved_account" || target.Mode == model.SupplyTargetAccount {
			target.LoginName = strings.TrimSpace(req.Username)
			if target.LoginName == "" {
				supplyError(c, model.ErrSupplyInput)
				return
			}
			target.Issuer, target.AccountAPI, target.UsageAPI, err = service.CheckCustomerAccount(c.Request.Context(), target, req.Password)
			if err != nil {
				common.ApiErrorT(c, err.Error())
				return
			}
			if c.Param("id") != "" && target.Mode == model.SupplyTargetAccount && previous.Issuer != target.Issuer {
				supplyError(c, model.ErrSupplyConflict)
				return
			}
			target.Mode = model.SupplyTargetAccount
			target.Secret, err = service.SealCustomerPassword(target, req.Password)
			if err != nil {
				common.ApiErrorT(c, "Configure a persistent encryption secret")
				return
			}
		} else {
			target.Issuer, err = service.VerifySupplyAccount(c.Request.Context(), target.URL, strings.TrimSpace(req.Username), req.Password)
			if err != nil {
				common.ApiErrorT(c, err.Error())
				return
			}
		}
		if c.Param("id") == "" {
			err = model.DB.Create(&target).Error
		} else {
			err = model.UpdateSupplyAccount(previous, target)
		}
		if err != nil {
			supplyError(c, err)
			return
		}
		common.ApiSuccess(c, gin.H{"id": id})
		return
	}
	if req.Token == "" {
		supplyError(c, model.ErrSupplyInput)
		return
	}
	secret, err := service.SealSupplySecret("target/"+id, req.Token)
	if err != nil {
		common.ApiErrorT(c, "Configure a persistent encryption secret")
		return
	}
	target := model.SupplyTarget{ID: id, Name: strings.TrimSpace(req.Name), URL: strings.TrimRight(req.URL, "/")}
	if c.Param("id") == "" {
		if !service.SupplyURL(target.URL) || len(target.Name) == 0 || len(target.Name) > 128 || len(target.URL) > 512 {
			supplyError(c, model.ErrSupplyInput)
			return
		}
		target.Issuer, err = service.CheckSupplyCredential(c.Request.Context(), target, req.Token)
		if err == nil {
			target.Secret = secret
			err = model.DB.Create(&target).Error
		}
	} else {
		if err = model.DB.First(&target, "id = ?", id).Error; err != nil {
			supplyError(c, err)
			return
		}
		if target.Mode == model.SupplyTargetDirect || target.Mode == model.SupplyTargetAccount {
			supplyError(c, model.ErrSupplyInput)
			return
		}
		issuer, probeErr := service.CheckSupplyCredential(c.Request.Context(), target, req.Token)
		if probeErr != nil {
			supplyError(c, probeErr)
			return
		}
		change := model.DB.Model(&model.SupplyTarget{}).Where("id = ? AND secret = ?", id, target.Secret).Updates(map[string]any{"secret": secret, "issuer": issuer})
		err = change.Error
		if err == nil && change.RowsAffected != 1 {
			err = model.ErrSupplyConflict
		}
	}
	if err != nil {
		supplyError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"id": id})
}

func ListSupplyResources(c *gin.Context) {
	q := model.DB.Omit("secret").Order("name,id")
	if owner := supplyOwnerScope(c, authz.ChannelRead); owner != 0 {
		q = q.Where("supplier_id = ?", owner)
	}
	var rows []model.SupplyResource
	if err := q.Find(&rows).Error; err != nil {
		supplyError(c, err)
		return
	}
	type resourceView struct {
		model.SupplyResource
		Assigned bool `json:"assigned"`
	}
	showTargets := canViewSupplyTargets(c)
	result := make([]resourceView, len(rows))
	ids := make([]string, 0, len(rows))
	for i, r := range rows {
		assigned := r.Target != ""
		if !showTargets {
			r.Target = ""
			r.UserID = 0
			r.CustomerGroup = ""
			if r.DirectState != model.SupplyAwaitRevocation {
				r.ActiveID = ""
			}
			if r.LastError != "" {
				r.LastError = "Operation failed"
			}
		}
		result[i] = resourceView{SupplyResource: r, Assigned: assigned}
		if showTargets && r.ActiveID != "" {
			ids = append(ids, r.ActiveID)
		}
	}
	if len(ids) > 0 {
		var current []model.SupplyAssignment
		if err := model.DB.Where("id IN ? AND (channel_id > 0 OR key_ref IS NOT NULL) AND ended = 0", ids).Find(&current).Error; err != nil {
			supplyError(c, err)
			return
		}
		byID := make(map[string]*model.SupplyAssignment, len(current))
		for i := range current {
			byID[current[i].ID] = &current[i]
		}
		for i := range result {
			result[i].Effective = byID[result[i].ActiveID]
		}
	}
	common.ApiSuccess(c, result)
}

func SaveSupplyResource(c *gin.Context) {
	var req struct {
		model.SupplyConfig
		Key      string `json:"key"`
		Revision int64  `json:"revision"`
	}
	if c.ShouldBindJSON(&req) != nil || !service.ValidSupplyConfig(req.SupplyConfig, false) || len(req.Key) > 48000 {
		supplyError(c, model.ErrSupplyInput)
		return
	}
	id := c.Param("id")
	creating := id == ""
	if creating {
		id = common.GetUUID()
		if strings.TrimSpace(req.Key) == "" {
			supplyError(c, model.ErrSupplyInput)
			return
		}
	}
	var secret string
	var err error
	if req.Key != "" {
		secret, err = service.SealSupplySecret("resource/"+id, req.Key)
		if err != nil {
			common.ApiErrorT(c, "Configure a persistent encryption secret")
			return
		}
	}
	actor := c.GetInt("id")
	if creating {
		r := model.SupplyResource{ID: id, SupplierID: actor, SupplyConfig: req.SupplyConfig, Secret: secret, Revision: 1, Applied: 1}
		err = model.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&r).Error; err != nil {
				return err
			}
			return model.SupplyAudit(tx, &r, actor, "created", "", 0)
		})
	} else {
		owner := supplyOwnerScope(c, authz.ChannelWrite)
		canEditSecrets := supplyOwnerScope(c, authz.ChannelSensitiveWrite) == 0
		err = model.EditSupply(id, owner, func(tx *gorm.DB, r *model.SupplyResource) error {
			if req.Revision != r.Revision || r.Archived {
				return model.ErrSupplyConflict
			}
			if r.SupplierID != actor && (req.Key != "" || req.BaseURL != r.BaseURL || req.Type != r.Type) && !canEditSecrets {
				return errSupplyForbidden
			}
			if r.ActiveID != "" {
				var assignment model.SupplyAssignment
				if err := tx.First(&assignment, "id = ?", r.ActiveID).Error; err != nil {
					return err
				}
				if assignment.KeyRef != nil && (req.Key != "" && common.GenerateHMAC(req.Key) != *assignment.KeyRef || r.BaseURL != req.BaseURL || r.Type != req.Type) {
					return errDirectRevokeFirst
				}
			}
			r.DirectState = ""
			r.SupplyConfig = req.SupplyConfig
			if secret != "" {
				r.Secret = secret
			}
			r.Revision++
			r.LastError = ""
			return model.SupplyAudit(tx, r, actor, "updated", r.Target, r.UserID)
		})
	}
	if err != nil {
		supplyError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"id": id})
}

func AssignSupply(c *gin.Context) {
	if !canViewSupplyTargets(c) {
		supplyError(c, errSupplyForbidden)
		return
	}
	var req struct {
		CustomerGroup string `json:"customer_group"`
		Scope         string `json:"scope"`
		Target        string `json:"target"`
		UserID        int    `json:"user_id"`
		Revision      int64  `json:"revision"`
	}
	if c.ShouldBindJSON(&req) != nil || req.UserID < 0 || (req.Target == "" && req.UserID != 0) {
		supplyError(c, model.ErrSupplyInput)
		return
	}
	err := model.EditSupply(c.Param("id"), 0, func(tx *gorm.DB, r *model.SupplyResource) error {
		if r.Revision != req.Revision || r.Archived {
			return model.ErrSupplyConflict
		}
		if req.Target != "" {
			var target model.SupplyTarget
			if err := model.LockSupplyTarget(tx, req.Target, &target); err != nil {
				return err
			}
			if target.Mode == model.SupplyTargetAccount {
				if target.AccountAPI != model.SupplyAPIOfficial {
					return service.ErrCustomerDenied
				}
				req.CustomerGroup = strings.TrimSpace(req.CustomerGroup)
				if req.CustomerGroup == "" {
					req.CustomerGroup = "default"
				}
				if len(req.CustomerGroup) > 64 || strings.ContainsAny(req.CustomerGroup, ",\r\n") {
					return model.ErrSupplyInput
				}
			} else {
				req.CustomerGroup = ""
			}
			if target.Mode == model.SupplyTargetDirect || target.Mode == model.SupplyTargetAccount {
				if req.UserID != 0 || req.Scope != "instance" {
					return model.ErrSupplyInput
				}
			} else if req.UserID <= 0 || req.Scope == "instance" {
				return model.ErrSupplyInput
			}
		}
		r.DirectState = ""
		r.Revision++
		r.CustomerGroup = req.CustomerGroup
		r.Target = req.Target
		r.UserID = req.UserID
		r.LastError = ""
		return model.SupplyAudit(tx, r, c.GetInt("id"), "assignment_requested", r.Target, r.UserID)
	})
	if err != nil {
		supplyError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

func WithdrawSupply(c *gin.Context) {
	revision, err := strconv.ParseInt(c.Query("revision"), 10, 64)
	if err != nil {
		supplyError(c, model.ErrSupplyInput)
		return
	}
	actor := c.GetInt("id")
	permission := authz.ChannelOperate
	if c.Request.Method == http.MethodDelete {
		permission = authz.ChannelSensitiveWrite
	}
	owner := supplyOwnerScope(c, permission)
	err = model.EditSupply(c.Param("id"), owner, func(tx *gorm.DB, r *model.SupplyResource) error {
		if r.Revision != revision || r.Archived {
			return model.ErrSupplyConflict
		}
		oldTarget, oldUser := r.Target, r.UserID
		action := "withdrawn"
		if c.Request.Method == "DELETE" {
			r.Archived = true
			r.Secret = ""
			action = "archived"
		}
		r.Revision++
		r.DirectState = ""
		r.Target = ""
		r.UserID = 0
		r.LastError = ""
		return model.SupplyAudit(tx, r, actor, action, oldTarget, oldUser)
	})
	if err != nil {
		supplyError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

func supplyResourceForReader(c *gin.Context) (model.SupplyResource, error) {
	var r model.SupplyResource
	q := model.DB.Omit("secret")
	if owner := supplyOwnerScope(c, authz.ChannelRead); owner != 0 {
		q = q.Where("supplier_id = ?", owner)
	}
	err := q.First(&r, "id = ?", c.Param("id")).Error
	return r, err
}

func SupplyHistory(c *gin.Context) {
	r, err := supplyResourceForReader(c)
	if err != nil {
		supplyError(c, err)
		return
	}
	var events []model.SupplyEvent
	if err = model.DB.Where("resource_id = ?", r.ID).Order("id DESC").Limit(200).Find(&events).Error; err != nil {
		supplyError(c, err)
		return
	}
	if !canViewSupplyTargets(c) {
		for i := range events {
			events[i].Target = ""
			events[i].UserID = 0
		}
	}
	common.ApiSuccess(c, events)
}
func SupplyUsage(c *gin.Context) {
	resource, err := supplyResourceForReader(c)
	if err != nil {
		supplyError(c, err)
		return
	}
	reportSupplyUsage(c, resource.ID)
}

// MySupplyUsage reports only channels supplied by the authenticated account,
// even when that account can administer other suppliers.
func MySupplyUsage(c *gin.Context) {
	reportSupplyUsage(c, "")
}

type ownSupplyUsageEntry struct {
	model.SupplyConsumption
	Day        int64  `json:"day"`
	ResourceID string `json:"resource_id"`
	Channel    string `json:"channel"`
}

func reportSupplyUsage(c *gin.Context, resourceID string) {
	own := resourceID == ""

	from, e1 := strconv.ParseInt(c.Query("from"), 10, 64)
	to, e2 := strconv.ParseInt(c.Query("to"), 10, 64)
	now := common.GetTimestamp()
	today := now / 86400 * 86400
	if e1 != nil || e2 != nil || from <= 0 || to < from || to > today || to-from >= 31*86400 || from%86400 != 0 || to%86400 != 0 {
		supplyError(c, model.ErrSupplyInput)
		return
	}
	savedDays := model.DB.Model(&model.SupplyWindow{}).Select("assignment_id").Where("day BETWEEN ? AND ?", from, to)
	assignmentsQuery := model.DB.WithContext(c.Request.Context()).Model(&model.SupplyAssignment{}).
		Where("(user_id > 0 OR account_api = ?)", model.SupplyAPIOfficial)
	names := map[string]string{}
	if own {
		resources := model.DB.Model(&model.SupplyResource{}).Select("id").Where("supplier_id = ?", c.GetInt("id"))
		assignmentsQuery = assignmentsQuery.Where("resource_id IN (?)", resources)
		var channels []struct{ ID, Name string }
		if err := model.DB.WithContext(c.Request.Context()).Model(&model.SupplyResource{}).Select("id", "name").Where("supplier_id = ?", c.GetInt("id")).Find(&channels).Error; err != nil {
			supplyError(c, err)
			return
		}
		for _, channel := range channels {
			names[channel.ID] = channel.Name
		}
	} else {
		assignmentsQuery = assignmentsQuery.Where("resource_id = ?", resourceID)
	}
	q := assignmentsQuery.Session(&gorm.Session{}).Where("started < ?", to+86400).
		Where("ended = 0 OR ended >= ? OR id IN (?)", from-86400, savedDays)
	showTargets := !own && canViewSupplyTargets(c)
	uid, target := c.Query("user_id"), c.Query("target")
	if !showTargets && (uid != "" || target != "") {
		supplyError(c, errSupplyForbidden)
		return
	}
	if uid != "" {
		v, e := strconv.Atoi(uid)
		if e != nil || v <= 0 {
			supplyError(c, model.ErrSupplyInput)
			return
		}
		q = q.Where("user_id = ?", v)
	}
	if target != "" {
		q = q.Where("target = ?", target)
	}
	var assignments []model.SupplyAssignment
	if err := q.Session(&gorm.Session{}).Order("started,id").Find(&assignments).Error; err != nil {
		supplyError(c, err)
		return
	}
	type entry struct {
		model.SupplyConsumption
		Day       int64  `json:"day"`
		Target    string `json:"target"`
		UserID    int    `json:"user_id"`
		ChannelID int    `json:"channel_id"`
	}
	rows := []entry{}
	ownRows := []ownSupplyUsageEntry{}
	complete := true
	asOf := int64(0)
	through := min(now, to+86400)
	var windows []model.SupplyWindow
	assignmentIDs := q.Session(&gorm.Session{}).Model(&model.SupplyAssignment{}).Select("id")
	if err := model.DB.WithContext(c.Request.Context()).Where("assignment_id IN (?) AND day BETWEEN ? AND ?", assignmentIDs, from, to).Find(&windows).Error; err != nil {
		supplyError(c, err)
		return
	}
	type windowKey struct {
		id  string
		day int64
	}
	cached := make(map[windowKey]model.SupplyWindow, len(windows))
	for _, w := range windows {
		cached[windowKey{w.AssignmentID, w.Day}] = w
	}
	modelFilter := c.Query("model")
	for _, a := range assignments {
		if own && names[a.ResourceID] == "" {
			continue
		}
		for day := max(from, a.Started/86400*86400); day <= to; day += 86400 {
			if c.Request.Context().Err() != nil {
				return
			}
			w := cached[windowKey{a.ID, day}]
			recentAssignment := a.Ended == 0 || day <= (a.Ended/86400+1)*86400
			if !recentAssignment && w.Collected == 0 {
				continue
			}
			if !own && recentAssignment && day == today {
				var err error
				w, err = service.CollectSupplyWindow(c.Request.Context(), a, day)
				if err != nil {
					complete = false
				}
			}
			if w.Collected == 0 || w.Incomplete || (day < today && w.Collected < day+86400) {
				complete = false
			}
			if w.Collected == 0 {
				continue
			}
			if own && day == today {
				through = min(through, w.Collected)
			}
			if asOf == 0 || w.Collected < asOf {
				asOf = w.Collected
			}
			var values []model.SupplyConsumption
			if err := common.UnmarshalJsonStr(w.Content, &values); err != nil {
				supplyError(c, err)
				return
			}
			for _, v := range values {
				if modelFilter != "" && v.ModelName == "" && a.AccountAPI == model.SupplyAPIOfficial {
					complete = false
					continue
				}
				if modelFilter == "" || modelFilter == v.ModelName {
					if own {
						ownRows = append(ownRows, ownSupplyUsageEntry{SupplyConsumption: v, Day: day, ResourceID: a.ResourceID, Channel: names[a.ResourceID]})
						continue
					}
					row := entry{SupplyConsumption: v, Day: day}
					if showTargets {
						row.Target, row.UserID, row.ChannelID = a.Target, a.UserID, a.ChannelID
					}
					rows = append(rows, row)
				}
			}
		}
	}
	var managed int64
	if err := assignmentsQuery.Count(&managed).Error; err != nil {
		supplyError(c, err)
		return
	}
	if own {
		common.ApiSuccess(c, gin.H{"rows": ownRows, "complete": complete, "as_of": asOf, "through": through, "has_windows": managed > 0})
		return
	}
	common.ApiSuccess(c, gin.H{"rows": rows, "complete": complete, "as_of": asOf, "has_windows": managed > 0})
}

func ReceiveSupply(c *gin.Context) {
	var cmd model.SupplyCommand
	id := c.Param("id")
	if c.ShouldBindJSON(&cmd) != nil || len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" || cmd.Version <= 0 || cmd.UserID <= 0 || !cmd.Retire && (!service.ValidSupplyConfig(cmd.Config, true) || len(cmd.Key) == 0 || len(cmd.Key) > 65536) {
		supplyError(c, model.ErrSupplyInput)
		return
	}
	receipt, err := model.ReceiveSupply(id, c.GetInt("id"), cmd)
	if err != nil {
		supplyError(c, err)
		return
	}
	if receipt.Retired && receipt.ChannelID != 0 {
		service.CloseActiveWebSocketsForChannel(receipt.ChannelID, service.ChannelDisabledCloseReason)
	}
	common.ApiSuccess(c, receipt)
}
func ReadSupplyWindow(c *gin.Context) {
	day, err := strconv.ParseInt(c.Query("day"), 10, 64)
	if err != nil || day <= 0 || day%86400 != 0 || day > common.GetTimestamp() {
		supplyError(c, model.ErrSupplyInput)
		return
	}
	var r model.SupplyReceipt
	if err = model.DB.Where("id = ? AND issuer = ?", c.Param("id"), c.GetInt("id")).First(&r).Error; err != nil {
		supplyError(c, err)
		return
	}
	if c.Query("next") == "true" {
		next, err := model.NextSupplyUsageDay(r, day)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "Usage unavailable"})
			return
		}
		common.ApiSuccess(c, next)
		return
	}
	rows, err := model.SupplyDayUsage(r, day)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "Usage unavailable"})
		return
	}
	common.ApiSuccess(c, rows)
}

func RetrySupply(c *gin.Context) {
	if err := service.ReconcileSupply(c.Request.Context(), c.Param("id")); err != nil {
		supplyError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

func SupplyReceiverIdentity(c *gin.Context) {
	if c.GetInt("access_token_id") <= 0 {
		supplyError(c, errSupplyForbidden)
		return
	}
	issuer := c.GetInt("id")
	if assignment := c.Query("assignment"); assignment != "" {
		var receipt model.SupplyReceipt
		if err := model.DB.Select("id").First(&receipt, "id = ? AND issuer = ?", assignment, issuer).Error; err != nil {
			supplyError(c, err)
			return
		}
	}
	common.ApiSuccess(c, issuer)
}

func ExportSupplyDelivery(c *gin.Context) {
	var input struct {
		Target   string `json:"target"`
		Revision int64  `json:"revision"`
	}
	if c.ShouldBindJSON(&input) != nil {
		supplyError(c, model.ErrSupplyInput)
		return
	}
	op := service.SupplyDeliveryContext{ResourceID: c.Param("id"), Target: input.Target, Revision: input.Revision}
	body, err := common.Marshal(op)
	if err != nil {
		supplyError(c, err)
		return
	}
	if middleware.RequireSecurityProof(c, service.VerificationOperation{Scope: service.VerificationScopeSupplyDelivery, Context: body}) == nil {
		return
	}
	data, err := service.ExportSupplyDelivery(op, c.GetInt("id"))
	if errors.Is(err, service.ErrDedicatedKey) {
		common.ApiErrorT(c, service.ErrDedicatedKey.Error())
		return
	}
	if err != nil {
		supplyError(c, err)
		return
	}
	common.ApiSuccess(c, data)
}
func ConfirmSupplyRevocation(c *gin.Context) {
	var req struct {
		Assignment string `json:"assignment"`
		Revision   int64  `json:"revision"`
		Revoked    bool   `json:"upstream_key_revoked"`
	}
	if c.ShouldBindJSON(&req) != nil || !req.Revoked {
		supplyError(c, model.ErrSupplyInput)
		return
	}
	if err := service.ConfirmSupplyRevocation(c.Param("id"), req.Assignment, req.Revision, c.GetInt("id"), supplyOwnerScope(c, authz.ChannelSensitiveWrite)); err != nil {
		supplyError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

func DirectProviderUsage(c *gin.Context) {
	r, err := supplyResourceForReader(c)
	if err != nil {
		supplyError(c, err)
		return
	}
	var rows []model.SupplyAssignment
	if err := model.DB.Where("resource_id = ? AND user_id = 0 AND key_ref IS NOT NULL", r.ID).Order("started,id").Find(&rows).Error; err != nil {
		supplyError(c, err)
		return
	}
	type sample struct {
		Target  string `json:"target"`
		Used    *int64 `json:"total_used"`
		At      int64  `json:"as_of"`
		Current bool   `json:"current"`
	}
	result := make([]sample, 0, len(rows))
	showTargets := canViewSupplyTargets(c)
	for _, a := range rows {
		fresh := false
		if a.Ended == 0 && a.ID == r.ActiveID {
			fresh = service.RefreshDirectProviderQuota(c.Request.Context(), &a) == nil
		}
		row := sample{Used: a.ProviderQuota, At: a.QuotaAt, Current: fresh}
		if showTargets {
			row.Target = a.Target
		}
		result = append(result, row)
	}
	common.ApiSuccess(c, result)
}
