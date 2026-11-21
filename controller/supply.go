package controller

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func supplyError(c *gin.Context, err error) {
	switch {
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
	var targets []model.SupplyTarget
	if err := model.DB.Omit("secret").Order("name").Find(&targets).Error; err != nil {
		supplyError(c, err)
		return
	}
	common.ApiSuccess(c, targets)
}
func SaveSupplyTarget(c *gin.Context) {
	var req struct{ Name, URL, Token string }
	if c.ShouldBindJSON(&req) != nil || len(req.Token) > 8192 || req.Token == "" {
		supplyError(c, model.ErrSupplyInput)
		return
	}
	id := c.Param("id")
	if id == "" {
		id = common.GetUUID()
	}
	secret, err := service.SealSupplySecret("target/"+id, req.Token)
	if err != nil {
		common.ApiErrorT(c, "Configure a persistent encryption secret")
		return
	}
	target := model.SupplyTarget{ID: id, Name: strings.TrimSpace(req.Name), URL: strings.TrimRight(req.URL, "/"), Secret: secret}
	if c.Param("id") == "" {
		if !service.SupplyURL(target.URL) || len(target.Name) == 0 || len(target.Name) > 128 || len(target.URL) > 512 {
			supplyError(c, model.ErrSupplyInput)
			return
		}
		err = model.DB.Create(&target).Error
	} else {
		// An existing target always denotes the same instance and its historical logs.
		change := model.DB.Model(&model.SupplyTarget{}).Where("id = ?", id).Update("secret", secret)
		err = change.Error
		if err == nil && change.RowsAffected == 0 {
			err = gorm.ErrRecordNotFound
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
	if c.GetInt("role") < common.RoleAdminUser {
		q = q.Where("supplier_id = ?", c.GetInt("id"))
	}
	var rows []model.SupplyResource
	if err := q.Find(&rows).Error; err != nil {
		supplyError(c, err)
		return
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.ActiveID != "" {
			ids = append(ids, r.ActiveID)
		}
	}
	if len(ids) > 0 {
		var current []model.SupplyAssignment
		if err := model.DB.Where("id IN ? AND channel_id > 0 AND ended = 0", ids).Find(&current).Error; err != nil {
			supplyError(c, err)
			return
		}
		byID := make(map[string]*model.SupplyAssignment, len(current))
		for i := range current {
			byID[current[i].ID] = &current[i]
		}
		for i := range rows {
			rows[i].Effective = byID[rows[i].ActiveID]
		}
	}
	common.ApiSuccess(c, rows)
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
		owner := actor
		if c.GetInt("role") >= common.RoleAdminUser {
			owner = 0
		}
		err = model.EditSupply(id, owner, func(tx *gorm.DB, r *model.SupplyResource) error {
			if req.Revision != r.Revision || r.Archived {
				return model.ErrSupplyConflict
			}
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
	var req struct {
		Target   string `json:"target"`
		UserID   int    `json:"user_id"`
		Revision int64  `json:"revision"`
	}
	if c.ShouldBindJSON(&req) != nil || req.UserID < 0 || (req.Target == "") != (req.UserID == 0) {
		supplyError(c, model.ErrSupplyInput)
		return
	}
	err := model.EditSupply(c.Param("id"), 0, func(tx *gorm.DB, r *model.SupplyResource) error {
		if r.Revision != req.Revision || r.Archived {
			return model.ErrSupplyConflict
		}
		if req.Target != "" {
			var target model.SupplyTarget
			if err := tx.Select("id").First(&target, "id = ?", req.Target).Error; err != nil {
				return err
			}
		}
		r.Revision++
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
	owner := actor
	if c.GetInt("role") >= common.RoleAdminUser {
		owner = 0
	}
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
	if c.GetInt("role") < common.RoleAdminUser {
		q = q.Where("supplier_id = ?", c.GetInt("id"))
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
	common.ApiSuccess(c, events)
}
func SupplyUsage(c *gin.Context) {
	resource, err := supplyResourceForReader(c)
	if err != nil {
		supplyError(c, err)
		return
	}
	from, e1 := strconv.ParseInt(c.Query("from"), 10, 64)
	to, e2 := strconv.ParseInt(c.Query("to"), 10, 64)
	today := common.GetTimestamp() / 86400 * 86400
	if e1 != nil || e2 != nil || from <= 0 || to < from || to > today || to-from >= 31*86400 || from%86400 != 0 || to%86400 != 0 {
		supplyError(c, model.ErrSupplyInput)
		return
	}
	q := model.DB.Where("resource_id = ? AND started < ?", resource.ID, to+86400)
	if uid := c.Query("user_id"); uid != "" {
		v, e := strconv.Atoi(uid)
		if e != nil || v <= 0 {
			supplyError(c, model.ErrSupplyInput)
			return
		}
		q = q.Where("user_id = ?", v)
	}
	if target := c.Query("target"); target != "" {
		q = q.Where("target = ?", target)
	}
	var assignments []model.SupplyAssignment
	if err = q.Order("started,id").Find(&assignments).Error; err != nil {
		supplyError(c, err)
		return
	}
	type entry struct {
		model.SupplyConsumption
		Day    int64  `json:"day"`
		Target string `json:"target"`
		UserID int    `json:"user_id"`
	}
	rows := []entry{}
	complete := true
	asOf := int64(0)
	for _, a := range assignments {
		for day := max(from, a.Started/86400*86400); day <= to; day += 86400 {
			if c.Request.Context().Err() != nil {
				return
			}
			w, e := service.CollectSupplyWindow(c.Request.Context(), a, day)
			if e != nil {
				complete = false
			}
			if w.Collected == 0 {
				continue
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
				if name := c.Query("model"); name == "" || name == v.ModelName {
					rows = append(rows, entry{v, day, a.Target, a.UserID})
				}
			}
		}
	}
	common.ApiSuccess(c, gin.H{"rows": rows, "complete": complete, "as_of": asOf})
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
