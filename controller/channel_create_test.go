package controller

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	gormlog "gorm.io/gorm/logger"
)

func setupChannelCreateTest(t *testing.T) int {
	t.Helper()
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousMaster, previousCache, previousRedis, previousSQLite := common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled, common.SQLitePath
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousType, previousLogType)
		common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled, common.SQLitePath = previousMaster, previousCache, previousRedis, previousSQLite
	})
	t.Setenv("SQL_DSN", os.Getenv("TEST_CHANNEL_SQL_DSN"))
	t.Setenv("LOG_SQL_DSN", "")
	common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled = false, false, false
	common.SQLitePath = filepath.Join(t.TempDir(), "channel.db")
	require.NoError(t, model.InitDB())
	database := model.DB
	sqlDB, err := database.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	model.LOG_DB = database
	common.SetLogDatabaseType(common.MainDatabaseType())
	require.NoError(t, database.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.User{}, &model.Log{}, &model.AuditLog{}))
	root := &model.User{Username: "channel-create-review-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled}
	require.NoError(t, database.Create(root).Error)
	t.Cleanup(func() { require.NoError(t, database.Unscoped().Delete(root).Error) })
	versionQuery := "SELECT VERSION()"
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		versionQuery = "SELECT sqlite_version()"
	}
	var version string
	require.NoError(t, database.Raw(versionQuery).Scan(&version).Error)
	t.Logf("database=%s version=%s", common.MainDatabaseType(), version)
	return root.Id
}

func TestAddChannelReturnsCreatedIDs(t *testing.T) {
	rootID := setupChannelCreateTest(t)
	const firstKey, secondKey = "sk-create-id-first", "sk-create-id-second"
	const vertexArray = `["", {"project_id":"one"}, " \n ", " {\"project_id\":\"two\"} ", {"project_id":"one"}, null, 42]`
	vertexKeys := []string{`{"project_id":"one"}`, `{"project_id":"two"}`, `{"project_id":"one"}`, "null", "42"}
	const vertexSingle = `{
  "project_id": "single",
  "type": "service_account"
}`
	batchKeys := make([]string, 51)
	batchIndices := make([]int, len(batchKeys))
	for i := range batchKeys {
		batchKeys[i] = fmt.Sprintf("sk-create-id-%02d", i)
		batchIndices[i] = i + 1
	}
	for _, tc := range []struct {
		name             string
		mode             string
		key              string
		channelType      int
		settings         string
		wantKeys         []string
		wantInputIndices []int
	}{
		{name: "single", mode: "single", key: firstKey, wantKeys: []string{firstKey}, wantInputIndices: []int{0}},
		{name: "batch duplicate keys", mode: "batch", key: "\n" + firstKey + "\n\n" + firstKey + "\n" + secondKey, wantKeys: []string{firstKey, firstKey, secondKey}, wantInputIndices: []int{1, 3, 4}},
		{name: "batch one channel", mode: "batch", key: firstKey + "\n", wantKeys: []string{firstKey}, wantInputIndices: []int{0}},
		{name: "batch preserves whitespace", mode: "batch", key: "\n \n" + firstKey, wantKeys: []string{" ", firstKey}, wantInputIndices: []int{1, 2}},
		{name: "multi to single", mode: "multi_to_single", key: "\n " + firstKey + " \n\n" + secondKey + "\n", wantKeys: []string{firstKey + "\n" + secondKey}, wantInputIndices: []int{1, 3}},
		{name: "multi preserves trimmed blank", mode: "multi_to_single", key: firstKey + "\n \n" + secondKey, wantKeys: []string{firstKey + "\n\n" + secondKey}, wantInputIndices: []int{0, 1, 2}},
		{name: "multi whitespace only", mode: "multi_to_single", key: " ", wantKeys: []string{}, wantInputIndices: []int{}},
		{name: "multiple chunks", mode: "batch", key: "\n" + strings.Join(batchKeys, "\n"), wantKeys: batchKeys, wantInputIndices: batchIndices},
		{name: "empty batch", mode: "batch", key: "\n", wantKeys: []string{}, wantInputIndices: []int{}},
		{name: "vertex array", mode: "batch", key: vertexArray, channelType: constant.ChannelTypeVertexAi, wantKeys: vertexKeys, wantInputIndices: []int{1, 3, 4, 5, 6}},
		{name: "vertex multi", mode: "multi_to_single", key: vertexArray, channelType: constant.ChannelTypeVertexAi, wantKeys: []string{strings.Join(vertexKeys, "\n")}, wantInputIndices: []int{1, 3, 4, 5, 6}},
		{name: "single multiline json", mode: "single", key: vertexSingle, channelType: constant.ChannelTypeVertexAi, wantKeys: []string{vertexSingle}, wantInputIndices: []int{0}},
		{name: "vertex api key batch", mode: "batch", key: "\n" + firstKey + "\n\n" + secondKey, channelType: constant.ChannelTypeVertexAi, settings: `{"vertex_key_type":"api_key"}`, wantKeys: []string{firstKey, secondKey}, wantInputIndices: []int{1, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "created-id-" + tc.name
			existing := &model.Channel{Name: name, Type: 1, Key: firstKey, Models: "test-model", Group: "default"}
			require.NoError(t, existing.Insert())
			t.Cleanup(func() {
				var ids []int
				require.NoError(t, model.DB.Model(&model.Channel{}).Where("name = ?", name).Pluck("id", &ids).Error)
				_, err := model.BatchDeleteChannels(ids)
				require.NoError(t, err)
			})
			channel := &model.Channel{Name: name, Type: tc.channelType, Key: tc.key, Models: "test-model", Group: "default", OtherSettings: tc.settings}
			if channel.Type == 0 {
				channel.Type = constant.ChannelTypeOpenAI
			}
			if channel.Type == constant.ChannelTypeVertexAi {
				channel.Other = `{"default":"us-central1"}`
			}
			request, err := common.Marshal(AddChannelRequest{Mode: tc.mode, Channel: channel})
			require.NoError(t, err)
			response := postAddChannel(t, rootID, common.RoleRootUser, string(request))
			require.Equal(t, http.StatusOK, response.Code)
			var result struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
				Data    struct {
					ID      *int  `json:"id"`
					IDs     []int `json:"ids"`
					Results []struct {
						InputIndex int `json:"input_index"`
						ChannelID  int `json:"channel_id"`
					} `json:"results"`
				} `json:"data"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			require.True(t, result.Success, response.Body.String())
			assert.Empty(t, result.Message)
			require.NotNil(t, result.Data.IDs, "empty creations must return an array, not null")
			require.Len(t, result.Data.IDs, len(tc.wantKeys))
			assert.NotContains(t, result.Data.IDs, existing.Id)
			var created []model.Channel
			require.NoError(t, model.DB.Where("name = ? AND id <> ?", name, existing.Id).Order("id").Find(&created).Error)
			require.Len(t, created, len(tc.wantKeys))
			for i, channel := range created {
				assert.Positive(t, result.Data.IDs[i])
				assert.Equal(t, channel.Id, result.Data.IDs[i])
				assert.Equal(t, tc.wantKeys[i], channel.Key)
				assert.Equal(t, tc.mode == "multi_to_single", channel.ChannelInfo.IsMultiKey)
			}
			require.NotNil(t, result.Data.Results, "empty creations must return an empty results array")
			require.Len(t, result.Data.Results, len(tc.wantInputIndices))
			results := make([]map[string]int, 0, len(tc.wantInputIndices))
			for i, inputIndex := range tc.wantInputIndices {
				channelIndex := i
				if tc.mode == "multi_to_single" {
					channelIndex = 0
				}
				assert.Equal(t, inputIndex, result.Data.Results[i].InputIndex)
				assert.Equal(t, created[channelIndex].Id, result.Data.Results[i].ChannelID)
				results = append(results, map[string]int{"input_index": inputIndex, "channel_id": created[channelIndex].Id})
			}
			if tc.mode == "multi_to_single" && len(created) == 1 {
				assert.Equal(t, len(tc.wantInputIndices), created[0].ChannelInfo.MultiKeySize)
			}
			data := map[string]any{"ids": result.Data.IDs, "results": results}
			if len(created) == 1 {
				require.NotNil(t, result.Data.ID)
				assert.Equal(t, created[0].Id, *result.Data.ID)
				data["id"] = created[0].Id
			} else {
				assert.Nil(t, result.Data.ID)
			}
			expected, err := common.Marshal(map[string]any{"success": true, "message": "", "data": data})
			require.NoError(t, err)
			assert.JSONEq(t, string(expected), response.Body.String(), "the response must contain only IDs, not channel credentials")
		})
	}
}

func TestAddChannelInvalidVertexBatchDoesNotReturnResults(t *testing.T) {
	rootID := setupChannelCreateTest(t)
	for _, mode := range []string{"batch", "multi_to_single"} {
		for _, key := range []string{"[]", `["", "  "]`, "[{"} {
			t.Run(mode+"/"+key, func(t *testing.T) {
				request, err := common.Marshal(AddChannelRequest{Mode: mode, Channel: &model.Channel{
					Name: "invalid-vertex-array", Type: constant.ChannelTypeVertexAi, Key: key,
					Models: "test-model", Group: "default", Other: `{"default":"us-central1"}`,
				}})
				require.NoError(t, err)
				response := postAddChannel(t, rootID, common.RoleRootUser, string(request))
				assert.Equal(t, http.StatusOK, response.Code)
				var result map[string]any
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
				assert.Equal(t, false, result["success"])
				assert.NotEmpty(t, result["message"])
				assert.NotContains(t, result, "data")
				var count int64
				require.NoError(t, model.DB.Model(&model.Channel{}).Where("name = ?", "invalid-vertex-array").Count(&count).Error)
				assert.Zero(t, count)
			})
		}
	}
}

func TestAddChannelRollbackDoesNotReturnIDs(t *testing.T) {
	rootID := setupChannelCreateTest(t)
	// Force a real database error in the second chunk, after the first 50 rows
	// received IDs. This constraint exists only in the isolated test database.
	column := `"key"`
	if common.UsingMainDatabase(common.DatabaseTypeMySQL) {
		column = "`key`(191)"
	}
	const indexName = "channel_create_test_unique_key"
	require.NoError(t, model.DB.Exec("CREATE UNIQUE INDEX "+indexName+" ON channels ("+column+")").Error)
	t.Cleanup(func() { require.NoError(t, model.DB.Migrator().DropIndex(&model.Channel{}, indexName)) })
	keys := make([]string, 51)
	for i := range keys[:50] {
		keys[i] = fmt.Sprintf("sk-create-rollback-%02d", i)
	}
	keys[50] = keys[0]
	request, err := common.Marshal(AddChannelRequest{Mode: "batch", Channel: &model.Channel{
		Name: "created-id-rollback", Type: 1, Key: strings.Join(keys, "\n"), Models: "test-model", Group: "default",
	}})
	require.NoError(t, err)
	response := postAddChannel(t, rootID, common.RoleRootUser, string(request))
	assert.Equal(t, http.StatusOK, response.Code)
	var result map[string]any
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	assert.Equal(t, false, result["success"])
	assert.NotEmpty(t, result["message"])
	assert.NotContains(t, result, "data")
	var channels, abilities int64
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("name = ?", "created-id-rollback").Count(&channels).Error)
	require.NoError(t, model.DB.Model(&model.Ability{}).Count(&abilities).Error)
	assert.Zero(t, channels)
	assert.Zero(t, abilities)
}

func TestAddChannelPanicRollsBackWithoutSuccess(t *testing.T) {
	rootID := setupChannelCreateTest(t)
	database := model.DB
	channelCreates := 0
	const callback = "test:channel-create-panic"
	require.NoError(t, database.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "channels" {
			channelCreates++
			if channelCreates == 2 {
				panic("second channel chunk failed")
			}
		}
	}))
	t.Cleanup(func() { require.NoError(t, database.Callback().Create().Remove(callback)) })
	keys := make([]string, 51)
	for i := range keys {
		keys[i] = fmt.Sprintf("sk-create-panic-%02d", i)
	}
	request, err := common.Marshal(AddChannelRequest{Mode: "batch", Channel: &model.Channel{
		Name: "created-id-panic", Type: 1, Key: strings.Join(keys, "\n"), Models: "test-model", Group: "default",
	}})
	require.NoError(t, err)
	require.PanicsWithValue(t, "second channel chunk failed", func() {
		postAddChannel(t, rootID, common.RoleRootUser, string(request))
	}, "a rolled-back transaction must not reach the success response")
	assert.Equal(t, 2, channelCreates)
	var channels, abilities int64
	require.NoError(t, database.Model(&model.Channel{}).Where("name = ?", "created-id-panic").Count(&channels).Error)
	require.NoError(t, database.Model(&model.Ability{}).Count(&abilities).Error)
	assert.Zero(t, channels)
	assert.Zero(t, abilities)
}

func TestAssignedChannelSelectionAndRevocation(t *testing.T) {
	require.NoError(t, i18n.Init())
	adminID := setupChannelCreateTest(t)
	owner := model.User{Username: "assigned-owner", AffCode: "asgnB", Status: common.UserStatusEnabled, Group: "default"}
	peer := model.User{Username: "assigned-peer", AffCode: "asgnD", Status: common.UserStatusEnabled, Group: "default"}
	require.NoError(t, model.DB.Create(&owner).Error)
	require.NoError(t, model.DB.Create(&peer).Error)
	t.Cleanup(func() {
		require.NoError(t, model.DB.Unscoped().Delete(&peer).Error)
		require.NoError(t, model.DB.Unscoped().Delete(&owner).Error)
	})
	const modelName = "assigned-test-model"
	ch := model.Channel{Name: "dedicated", Type: constant.ChannelTypeOpenAI, Models: modelName, Group: "default", Key: "fixture", AssignedUserID: &owner.Id, Priority: common.GetPointer(int64(100))}
	body, err := common.Marshal(AddChannelRequest{Mode: "single", Channel: &ch})
	require.NoError(t, err)
	rec := postAddChannel(t, adminID, common.RoleRootUser, string(body))
	var result struct {
		Success bool
		Data    struct{ ID int }
	}
	require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &result))
	require.True(t, result.Success, rec.Body.String())
	require.Positive(t, result.Data.ID)
	shared := &model.Channel{Name: "shared", Type: ch.Type, Models: modelName, Group: "default", Key: "shared-fixture", Status: common.ChannelStatusEnabled}
	require.NoError(t, shared.Insert())
	t.Cleanup(func() {
		_, err := model.BatchDeleteChannels([]int{result.Data.ID, shared.Id})
		require.NoError(t, err)
	})
	for _, cached := range []bool{false, true} {
		common.MemoryCacheEnabled = cached
		if cached {
			model.InitChannelCache()
		}
		for _, tc := range []struct {
			name                 string
			uid, attempt, wantID int
			pin                  bool
		}{
			{"owner", owner.Id, 0, result.Data.ID, false},
			{"peer fallback", peer.Id, 0, shared.Id, false},
			{"peer retry", peer.Id, 1, shared.Id, false},
			{"owner pin", owner.Id, 0, result.Data.ID, true},
			{"peer pin", peer.Id, 0, 0, true},
			{"missing uid pin", 0, 0, 0, true},
		} {
			t.Run(fmt.Sprintf("cache=%t/%s", cached, tc.name), func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				common.SetContextKey(c, constant.ContextKeyUserId, tc.uid)
				if tc.pin {
					service.GetChannelConstraints(c).AddPin(dto.ChannelPin{Source: dto.PinSourceToken, ChannelId: result.Data.ID})
				}
				selected, _, selectErr := service.SelectChannelForRequest(c, modelName, &service.RetryParam{Ctx: c, ModelName: modelName, TokenGroup: "default", Retry: &tc.attempt})
				if tc.wantID == 0 {
					require.Nil(t, selected)
					require.NotNil(t, selectErr)
					return
				}
				require.Nil(t, selectErr)
				require.NotNil(t, selected)
				assert.Equal(t, tc.wantID, selected.Id)
				require.Nil(t, middleware.SetupContextForSelectedChannel(c, selected, modelName))
			})
		}
	}
	stale, err := model.CacheGetChannel(result.Data.ID)
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", stale.Id).Update("status", common.ChannelStatusManuallyDisabled).Error)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyUserId, owner.Id)
	setupErr := middleware.SetupContextForSelectedChannel(c, stale, modelName)
	require.NotNil(t, setupErr)
	assert.Equal(t, http.StatusForbidden, setupErr.StatusCode)
	assert.Empty(t, common.GetContextKeyString(c, constant.ContextKeyChannelKey))
}

func TestSupplyReceiptAndUsage(t *testing.T) {
	require.NoError(t, i18n.Init())
	issuer := setupChannelCreateTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.SupplyReceipt{}))
	id := common.GetUUID()
	t.Cleanup(func() { require.NoError(t, model.DB.Where("id = ?", id).Delete(&model.SupplyReceipt{}).Error) })
	cmd := model.SupplyCommand{UserID: issuer, Version: 1, Key: "mock-credential", Config: model.SupplyConfig{Name: "dedicated", Type: 1, Models: "gpt-4o"}}
	var debugSQL bytes.Buffer
	db := model.DB
	model.DB = db.Session(&gorm.Session{Logger: gormlog.New(log.New(&debugSQL, "", 0), gormlog.Config{LogLevel: gormlog.Info})})
	t.Cleanup(func() { model.DB = db })
	first, err := model.ReceiveSupply(id, issuer, cmd)
	require.NoError(t, err)
	require.Positive(t, first.ChannelID)
	replayed, err := model.ReceiveSupply(id, issuer, cmd)
	require.NoError(t, err)
	assert.Equal(t, first.ChannelID, replayed.ChannelID)
	conflict := cmd
	conflict.Key = "conflicting"
	_, err = model.ReceiveSupply(id, issuer, conflict)
	require.ErrorIs(t, err, model.ErrSupplyConflict)
	_, err = model.ReceiveSupply(id, issuer+1, cmd)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", first.ChannelID).Update("status", common.ChannelStatusAutoDisabled).Error)
	cmd.Version = 2
	cmd.Key = "rotated"
	updated, err := model.ReceiveSupply(id, issuer, cmd)
	require.NoError(t, err)
	assert.Equal(t, first.ChannelID, updated.ChannelID)
	assert.NotContains(t, debugSQL.String(), "mock-credential")
	assert.NotContains(t, debugSQL.String(), "rotated")
	ch, err := model.GetChannelById(first.ChannelID, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, ch.Status)
	_, err = ch.ActiveForUser(issuer)
	require.Error(t, err)
	probe, err := ch.ForHealthProbe()
	require.NoError(t, err)
	assert.Equal(t, cmd.Key, probe.Key)
	probeCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	probeCtx.Request = httptest.NewRequest("POST", "/", nil)
	probeCtx.Set("id", issuer+1)
	require.Nil(t, middleware.SetupContextForChannelProbe(probeCtx, ch, "gpt-4o"))
	assert.Equal(t, issuer+1, probeCtx.GetInt("id"))
	assert.Equal(t, cmd.Key, common.GetContextKeyString(probeCtx, constant.ContextKeyChannelKey))
	require.NoError(t, db.Model(ch).Update("status", common.ChannelStatusEnabled).Error)
	savedRoute, err := ch.ActiveForUser(issuer)
	require.NoError(t, err)
	require.True(t, savedRoute.ServesSupplyRoute("default", "gpt-4o"))
	require.NoError(t, savedRoute.CheckRealtimeBinding(issuer, "default", "gpt-4o"))
	cmd.Version++
	cmd.Config.Models = "replacement-model"
	_, err = model.ReceiveSupply(id, issuer, cmd)
	require.NoError(t, err)
	current, err := savedRoute.ActiveForUser(issuer)
	require.NoError(t, err)
	assert.False(t, current.ServesSupplyRoute("default", "gpt-4o"))
	assert.False(t, current.ServesSupplyRoute("other", "replacement-model"))
	require.Error(t, savedRoute.CheckRealtimeBinding(issuer, "default", "gpt-4o"))
	staleCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	staleCtx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	staleCtx.Set("id", issuer)
	staleCtx.Set("group", "default")
	require.NotNil(t, middleware.SetupContextForSelectedChannel(staleCtx, savedRoute, "gpt-4o"))
	assert.Empty(t, common.GetContextKeyString(staleCtx, constant.ContextKeyChannelKey))
	day := common.GetTimestamp() / 86400 * 86400
	log := model.Log{UserId: issuer, ChannelId: first.ChannelID, Type: model.LogTypeConsume, ModelName: "gpt-4o", CreatedAt: day + 1, PromptTokens: 12, CompletionTokens: 4}
	require.NoError(t, model.LOG_DB.Create(&log).Error)
	t.Cleanup(func() { require.NoError(t, model.LOG_DB.Delete(&log).Error) })
	cmd.Version++
	cmd.Retire = true
	receipt, err := model.ReceiveSupply(id, issuer, cmd)
	require.NoError(t, err)
	require.True(t, receipt.Retired)
	bound, err := model.GetChannelById(first.ChannelID, true)
	require.NoError(t, err)
	assert.Empty(t, bound.Key)
	require.Error(t, current.CheckRealtimeBinding(issuer, "default", "replacement-model"))
	_, err = bound.ForHealthProbe()
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	require.NoError(t, model.DB.Model(bound).Update("status", common.ChannelStatusEnabled).Error)
	_, err = bound.ActiveForUser(issuer)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	_, err = model.BatchDeleteChannels([]int{first.ChannelID})
	require.ErrorIs(t, err, model.ErrSupplyConflict)
	t.Cleanup(func() { require.NoError(t, model.DB.Where("id = ?", first.ChannelID).Delete(&model.Channel{}).Error) })
	cmd.Version++
	cmd.Retire = false
	_, err = model.ReceiveSupply(id, issuer, cmd)
	require.ErrorIs(t, err, model.ErrSupplyConflict)
	next, err := model.NextSupplyUsageDay(receipt, day)
	require.NoError(t, err)
	assert.Equal(t, day, next)
	rows, err := model.SupplyDayUsage(receipt, day)
	require.NoError(t, err)
	assert.Equal(t, []model.SupplyConsumption{{ModelName: "gpt-4o", Records: 1, InputTokens: 12, OutputTokens: 4}}, rows)
}

func TestSupplyReviewSafety(t *testing.T) {
	owner := setupChannelCreateTest(t)
	db := model.DB
	require.NoError(t, db.AutoMigrate(&model.SupplyResource{}, &model.SupplyTarget{}, &model.SupplyAssignment{}, &model.SupplyWindow{}, &model.SupplyEvent{}))
	t.Setenv("CRYPTO_SECRET", "fixture-only")
	t.Run("only a valid single channel can be removed", func(t *testing.T) {
		ch := model.Channel{Name: "delete-guard", Key: "fixture", Group: "default", Models: "guard"}
		require.NoError(t, ch.Insert())
		for _, id := range []string{"0", "invalid", "-1"} {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Params = gin.Params{{Key: "id", Value: id}}
			DeleteChannel(c)
			assert.Contains(t, rec.Body.String(), `"success":false`)
		}
		require.Error(t, (&model.Channel{}).Delete())
		_, err := model.GetChannelById(ch.Id, false)
		require.NoError(t, err)
		require.NoError(t, ch.Delete())
	})
	t.Run("visibility and shared polling cursor", func(t *testing.T) {
		ch := model.Channel{Name: "bound-keys", Key: "one\ntwo", Group: "default", Models: "private-fixture", AssignedUserID: &owner, Status: common.ChannelStatusEnabled, ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}}
		require.NoError(t, ch.Insert())
		t.Cleanup(func() { require.NoError(t, ch.Delete()) })
		assert.Contains(t, model.GetGroupEnabledModels("default", owner), "private-fixture")
		assert.NotContains(t, model.GetGroupEnabledModels("default", owner+1), "private-fixture")
		common.MemoryCacheEnabled = true
		defer func() { common.MemoryCacheEnabled = false }()
		model.InitChannelCache()
		for _, expected := range []string{"one", "two", "one"} {
			current, err := ch.ActiveForUser(owner)
			require.NoError(t, err)
			key, _, apiErr := current.GetNextEnabledKey()
			require.Nil(t, apiErr)
			assert.Equal(t, expected, key)
		}
	})
	t.Run("credential preflight and historic window preservation", func(t *testing.T) {
		identity := owner
		valid := true
		calls := 0
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/identity") {
				if !valid {
					w.WriteHeader(403)
					return
				}
				fmt.Fprintf(w, `{"success":true,"data":%d}`, identity)
				return
			}
			calls++
			fmt.Fprint(w, `{"success":true,"data":[]}`)
		}))
		defer peer.Close()
		target := model.SupplyTarget{ID: common.GetUUID(), Name: "fixture", URL: peer.URL, Issuer: owner}
		key, err := service.SealSupplySecret("target/"+target.ID, "fixture")
		require.NoError(t, err)
		target.Secret = key
		require.NoError(t, db.Create(&target).Error)
		t.Cleanup(func() { require.NoError(t, db.Delete(&target).Error) })
		rotate := func() bool {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Params = gin.Params{{Key: "id", Value: target.ID}}
			c.Request = httptest.NewRequest("PUT", "/", strings.NewReader(`{"token":"replacement"}`))
			SaveSupplyTarget(c)
			return strings.Contains(rec.Body.String(), `"success":true`)
		}
		identity++
		assert.False(t, rotate())
		valid = false
		assert.False(t, rotate())
		var saved model.SupplyTarget
		require.NoError(t, db.First(&saved, "id = ?", target.ID).Error)
		assert.Equal(t, key, saved.Secret)
		identity = owner
		valid = true
		assert.True(t, rotate())
		r := model.SupplyResource{ID: common.GetUUID(), SupplierID: owner, Revision: 1}
		require.NoError(t, db.Create(&r).Error)
		t.Cleanup(func() { require.NoError(t, db.Delete(&r).Error) })
		day := (common.GetTimestamp()/86400 - 40) * 86400
		a := model.SupplyAssignment{ID: common.GetUUID(), ResourceID: r.ID, Target: target.ID, UserID: owner, ChannelID: 123, Started: day, Ended: day + 3600}
		require.NoError(t, db.Create(&a).Error)
		t.Cleanup(func() { require.NoError(t, db.Delete(&a).Error) })
		w := model.SupplyWindow{AssignmentID: a.ID, Day: day, Collected: day + 3600, Content: `[{"model":"sample","records":10,"input_tokens":12}]`}
		require.NoError(t, db.Create(&w).Error)
		t.Cleanup(func() { require.NoError(t, db.Delete(&w).Error) })
		observed, err := service.CollectSupplyWindow(t.Context(), a, day)
		require.NoError(t, err)
		assert.True(t, observed.Incomplete)
		assert.Equal(t, w.Content, observed.Content)
		var stored model.SupplyWindow
		require.NoError(t, db.First(&stored, "assignment_id = ? AND day = ?", a.ID, day).Error)
		assert.Equal(t, w.Collected, stored.Collected)
		assert.True(t, stored.Incomplete)
		for _, filterDay := range []int64{day, common.GetTimestamp() / 86400 * 86400} {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Set("id", owner)
			c.Set("role", common.RoleCommonUser)
			c.Params = gin.Params{{Key: "id", Value: r.ID}}
			c.Request = httptest.NewRequest("GET", fmt.Sprintf("/?from=%d&to=%d", filterDay, filterDay), nil)
			SupplyUsage(c)
			assert.Contains(t, rec.Body.String(), `"success":true`)
			if filterDay == day {
				assert.Contains(t, rec.Body.String(), `"records":10`)
				assert.Contains(t, rec.Body.String(), `"complete":false`)
			}
		}
		assert.Equal(t, 1, calls)
		// A long-running request may settle after the ordinary grace period.
		// The history page must include a known late window without contacting the retired target.
		late := model.SupplyWindow{AssignmentID: a.ID, Day: day + 5*86400, Collected: day + 8*86400, Content: `[{"model":"late","records":2}]`}
		require.NoError(t, db.Create(&late).Error)
		t.Cleanup(func() { require.NoError(t, db.Delete(&late).Error) })
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Set("id", owner)
		c.Params = gin.Params{{Key: "id", Value: r.ID}}
		c.Request = httptest.NewRequest("GET", fmt.Sprintf("/?from=%d&to=%d", late.Day, late.Day), nil)
		SupplyUsage(c)
		assert.Contains(t, rec.Body.String(), `"records":2`)
		assert.Contains(t, rec.Body.String(), `"complete":true`)
		assert.Equal(t, 1, calls)
	})
}

func TestSupplyRestrictedAdminCannotChangeOtherSupplier(t *testing.T) {
	setupTaskPluginBindChannelTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.SupplyResource{}, &model.SupplyEvent{}))
	cfg := model.SupplyConfig{Name: "original", Models: "model", Type: 1}
	r := model.SupplyResource{ID: common.GetUUID(), SupplierID: 91, Revision: 1, SupplyConfig: cfg}
	require.NoError(t, model.DB.Create(&r).Error)
	const admin = 92
	require.NoError(t, authz.SetUserPermissions(admin, authz.PermissionsMap{authz.ResourceChannel: {authz.ActionRead: true, authz.ActionWrite: false, authz.ActionOperate: false, authz.ActionSensitiveWrite: false}}))
	for _, tc := range []struct {
		method  string
		handler gin.HandlerFunc
	}{{"PUT", SaveSupplyResource}, {"POST", WithdrawSupply}, {"DELETE", WithdrawSupply}} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Set("id", admin)
		c.Set("role", common.RoleAdminUser)
		c.Params = gin.Params{{Key: "id", Value: r.ID}}
		c.Request = httptest.NewRequest(tc.method, "/?revision=1", strings.NewReader(`{"name":"changed","type":1,"models":"model","revision":1}`))
		tc.handler(c)
		assert.Contains(t, rec.Body.String(), `"success":false`)
	}
	require.NoError(t, authz.SetUserPermissions(admin, authz.PermissionsMap{authz.ResourceChannel: {authz.ActionRead: true, authz.ActionWrite: true, authz.ActionSensitiveWrite: false}}))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set("id", admin)
	c.Set("role", common.RoleAdminUser)
	c.Params = gin.Params{{Key: "id", Value: r.ID}}
	c.Request = httptest.NewRequest("PUT", "/", strings.NewReader(`{"name":"changed","type":1,"models":"model","base_url":"https://example.org","revision":1}`))
	SaveSupplyResource(c)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	var saved model.SupplyResource
	require.NoError(t, model.DB.First(&saved, "id = ?", r.ID).Error)
	assert.Equal(t, int64(1), saved.Revision)
	assert.False(t, saved.Archived)
}

func supplyTestSocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("websocket handshake: %v", err)
			return
		}
		accepted <- conn
	}))
	t.Cleanup(server.Close)
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	peer := <-accepted
	t.Cleanup(func() { client.Close(); peer.Close() })
	return client, peer
}

func TestSupplyRealtimeRevocation(t *testing.T) {
	uid := setupChannelCreateTest(t)
	db := model.DB
	require.NoError(t, db.AutoMigrate(&model.SupplyReceipt{}))
	id := common.GetUUID()
	cmd := model.SupplyCommand{UserID: uid, Version: 1, Key: "fixture", Config: model.SupplyConfig{Name: "realtime", Models: "rt-model", Type: constant.ChannelTypeOpenAI}}
	receipt, err := model.ReceiveSupply(id, uid, cmd)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Where("id = ?", id).Delete(&model.SupplyReceipt{}).Error)
		require.NoError(t, db.Where("id = ?", receipt.ChannelID).Delete(&model.Channel{}).Error)
	})
	ch, err := model.GetChannelById(receipt.ChannelID, true)
	require.NoError(t, err)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("id", uid)
	c.Set("group", "default")
	c.Request = httptest.NewRequest("GET", "/v1/realtime", nil)
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, ch, "rt-model"))
	client, downstream := supplyTestSocketPair(t)
	upstream, source := supplyTestSocketPair(t)
	info := &relaycommon.RelayInfo{UserId: uid, UsingGroup: "default", OriginModelName: "rt-model", ClientWs: downstream, TargetWs: upstream, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "rt-model"}}
	done := make(chan struct{})
	go func() { defer close(done); defer upstream.Close(); openai.OpenaiRealtimeHandler(c, info) }()
	event := []byte(`{"type":"response.create"}`)
	require.NoError(t, client.WriteMessage(websocket.TextMessage, event))
	require.NoError(t, source.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, data, err := source.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, event, data)
	// Commit a target-side revocation while the relay on another node still
	// holds its original connection and has received no close notification.
	cmd.Version++
	cmd.Retire = true
	_, err = model.ReceiveSupply(id, uid, cmd)
	require.NoError(t, err)
	require.NoError(t, client.WriteMessage(websocket.TextMessage, event))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("revoked session continued accepting events")
	}
	_, _, err = source.ReadMessage()
	require.Error(t, err)
}

func TestDirectSupplyCustomerHandoff(t *testing.T) {
	owner := setupChannelCreateTest(t)
	db := model.DB
	t.Setenv("CRYPTO_SECRET", "direct-test-only")
	require.NoError(t, db.AutoMigrate(&model.SupplyResource{}, &model.SupplyAssignment{}, &model.SupplyTarget{}, &model.SupplyEvent{}))
	r := model.SupplyResource{ID: common.GetUUID(), SupplierID: owner, Revision: 1, Applied: 1, SupplyConfig: model.SupplyConfig{Name: "direct-test", Type: constant.ChannelTypeNewAPI, Models: "demo", BaseURL: "https://supply.example.org"}}
	oldKey := common.GetUUID()
	var err error
	r.Secret, err = service.SealSupplySecret("resource/"+r.ID, oldKey)
	require.NoError(t, err)
	require.NoError(t, db.Create(&r).Error)
	t1 := model.SupplyTarget{ID: common.GetUUID(), Name: "one", Mode: model.SupplyTargetDirect}
	t2 := model.SupplyTarget{ID: common.GetUUID(), Name: "two", Mode: model.SupplyTargetDirect}
	require.NoError(t, db.Create(&t1).Error)
	require.NoError(t, db.Create(&t2).Error)
	t.Cleanup(func() {
		require.NoError(t, db.Where("resource_id = ?", r.ID).Delete(&model.SupplyAssignment{}).Error)
		require.NoError(t, db.Where("resource_id = ?", r.ID).Delete(&model.SupplyEvent{}).Error)
		require.NoError(t, db.Delete(&r).Error)
		require.NoError(t, db.Delete(&t1).Error)
		require.NoError(t, db.Delete(&t2).Error)
	})
	assign := func(target string, rev int64, uid int) bool {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Set("role", common.RoleRootUser)
		c.Set("id", owner)
		c.Params = gin.Params{{Key: "id", Value: r.ID}}
		c.Request = httptest.NewRequest("PUT", "/", strings.NewReader(fmt.Sprintf(`{"scope":"instance","target":%q,"revision":%d,"user_id":%d}`, target, rev, uid)))
		AssignSupply(c)
		return strings.Contains(rec.Body.String(), `"success":true`)
	}
	assert.False(t, assign(t1.ID, 1, owner))
	require.True(t, assign(t1.ID, 1, 0))
	require.NoError(t, service.ReconcileSupply(t.Context(), r.ID))
	op := service.SupplyDeliveryContext{ResourceID: r.ID, Target: t1.ID, Revision: 2}
	out, err := service.ExportSupplyDelivery(op, owner)
	require.NoError(t, err)
	assert.Equal(t, oldKey, out.Key)
	out, err = service.ExportSupplyDelivery(op, owner)
	require.NoError(t, err)
	assert.Equal(t, r.BaseURL, out.BaseURL)
	require.True(t, assign(t2.ID, 2, 0))
	require.NoError(t, service.ReconcileSupply(t.Context(), r.ID))
	var pending model.SupplyResource
	require.NoError(t, db.First(&pending, "id = ?", r.ID).Error)
	assert.Equal(t, model.SupplyAwaitRevocation, pending.DirectState)
	_, err = service.ExportSupplyDelivery(op, owner)
	require.ErrorIs(t, err, model.ErrSupplyConflict)
	require.ErrorIs(t, service.ConfirmSupplyRevocation(r.ID, pending.ActiveID, 2, owner, owner), model.ErrSupplyConflict)
	require.NoError(t, service.ConfirmSupplyRevocation(r.ID, pending.ActiveID, 3, owner, owner))
	require.NoError(t, db.Model(&r).Update("secret", r.Secret).Error)
	require.NoError(t, service.ReconcileSupply(t.Context(), r.ID))
	op.Target = t2.ID
	op.Revision = 3
	_, err = service.ExportSupplyDelivery(op, owner)
	require.ErrorIs(t, err, service.ErrDedicatedKey)
	newKey, err := service.SealSupplySecret("resource/"+r.ID, "new-"+oldKey)
	require.NoError(t, err)
	require.NoError(t, db.Model(&r).Update("secret", newKey).Error)
	out, err = service.ExportSupplyDelivery(op, owner)
	require.NoError(t, err)
	assert.Equal(t, "new-"+oldKey, out.Key)
}

func TestDirectSupplyOrdinaryAccountRegistration(t *testing.T) {
	setupChannelCreateTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.SupplyTarget{}))
	uid := 7
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status":
			fmt.Fprint(w, `{"success":true,"data":{}}`)
		case "/api/user/login":
			fmt.Fprintf(w, `{"success":true,"data":{"user":{"id":%d},"access_token":"fixture-session"}}`, uid)
		case "/api/user/self":
			fmt.Fprintf(w, `{"success":true,"data":{"id":%d,"username":"ordinary","role":1}}`, uid)
		case "/api/user/auth/logout":
			fmt.Fprint(w, `{"success":true}`)
		default:
			t.Errorf("unexpected downstream operation %s", r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer site.Close()
	save := func(id string) bool {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Params = gin.Params{{Key: "id", Value: id}}
		payload, err := common.Marshal(map[string]string{"url": site.URL + "/", "username": "ordinary", "password": "fixture-password"})
		require.NoError(t, err)
		c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload))
		SaveSupplyTarget(c)
		assert.NotContains(t, rec.Body.String(), "fixture-password")
		assert.NotContains(t, rec.Body.String(), "fixture-session")
		return strings.Contains(rec.Body.String(), `"success":true`)
	}
	require.True(t, save(""))
	var target model.SupplyTarget
	require.NoError(t, model.DB.First(&target, "url = ?", site.URL).Error)
	t.Cleanup(func() { require.NoError(t, model.DB.Delete(&target).Error) })
	assert.Equal(t, model.SupplyTargetDirect, target.Mode)
	assert.Empty(t, target.Secret)
	assert.Equal(t, uid, target.Issuer)
	assert.False(t, save(""))
	uid++
	require.True(t, save(target.ID))
	require.NoError(t, model.DB.First(&target, "id = ?", target.ID).Error)
	assert.Equal(t, uid, target.Issuer)
	assert.Empty(t, target.Secret)
}

// Stock channel creation may commit before B receives an acknowledgement.
// The same persisted assignment must recover the original ID without another POST.
func TestCustomerSupplyUncertainWriteAndScopedUsage(t *testing.T) {
	owner := setupChannelCreateTest(t)
	t.Setenv("CRYPTO_SECRET", "customer-test-only")
	db := model.DB
	require.NoError(t, db.AutoMigrate(&model.SupplyTarget{}, &model.SupplyResource{}, &model.SupplyAssignment{}, &model.SupplyEvent{}, &model.SupplyWindow{}))
	var visible atomic.Bool
	var creates atomic.Int64
	var remote atomic.Value
	day := (common.GetTimestamp()/86400 - 1) * 86400
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/status":
			fmt.Fprint(w, `{"success":true,"data":{}}`)
		case "/api/user/login":
			fmt.Fprint(w, `{"success":true,"data":{"id":7,"access_token":"fixture"}}`)
		case "/api/user/self":
			fmt.Fprint(w, `{"success":true,"data":{"id":7,"username":"customer"}}`)
		case "/api/channel/search":
			rows := []any{}
			if row := remote.Load(); visible.Load() && row != nil {
				rows = append(rows, row)
			}
			raw, err := common.Marshal(rows)
			if !assert.NoError(t, err) {
				return
			}
			fmt.Fprintf(w, `{"success":true,"data":{"items":%s,"total":%d}}`, raw, len(rows))
		case "/api/channel/":
			if !assert.Equal(t, "POST", req.Method) {
				return
			}
			var payload struct{ Channel map[string]any }
			if !assert.NoError(t, common.DecodeJson(req.Body, &payload)) {
				return
			}
			delete(payload.Channel, "key")
			payload.Channel["id"] = 53
			payload.Channel["status"] = 1
			remote.Store(payload.Channel)
			creates.Add(1)
			// A stored the channel but its acknowledgement was truncated.
			fmt.Fprint(w, `{"success":`)
		case "/api/data/channel_daily":
			assert.Equal(t, "log", req.URL.Query().Get("source"))
			assert.Equal(t, "0", req.URL.Query().Get("tz_offset"))
			fmt.Fprintf(w, `{"success":true,"data":[{"channel_id":99,"count":77},{"channel_id":53,"day":%d,"count":2,"prompt_tokens":12,"completion_tokens":3,"quota":4,"cache_read_tokens":0}]}`, day)
		case "/api/log/":
			if req.URL.Query().Get("start_timestamp") == fmt.Sprint(day-2*86400) {
				timestamp := day - 2*86400
				fmt.Fprintf(w, `{"success":true,"data":{"total":3,"items":[
                    {"channel":53,"type":2,"created_at":%d,"model_name":"recorded","prompt_tokens":10,"completion_tokens":1,"quota":1,"other":"{\"cache_tokens\":0,\"cache_creation_tokens\":5,\"cache_creation_tokens_5m\":3,\"cache_creation_tokens_1h\":2}"},
                    {"channel":53,"type":2,"created_at":%d,"model_name":"recorded","prompt_tokens":10,"completion_tokens":1,"quota":1,"other":{"cache_tokens":2,"cache_write_tokens":7,"cache_creation_tokens_12h":4}},
                    {"channel":53,"type":2,"created_at":%d,"model_name":"read-only","prompt_tokens":10,"completion_tokens":1,"quota":1,"other":"{\"cache_tokens\":0}"}
                ]}}`, timestamp, timestamp, timestamp)
				return
			}
			// A misconfigured server ignores the channel filter.
			fmt.Fprintf(w, `{"success":true,"data":{"total":1,"items":[{"channel":99,"type":2,"created_at":%d,"model_name":"demo","prompt_tokens":1,"completion_tokens":1,"quota":1}]}}`, day)
		default:
			t.Errorf("unexpected request %s", req.URL.Path)
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer site.Close()
	target := model.SupplyTarget{ID: common.GetUUID(), Mode: model.SupplyTargetAccount, URL: site.URL, LoginName: "customer", Issuer: 7, AccountAPI: model.SupplyAPIOfficial, UsageAPI: "channel_daily"}
	var err error
	target.Secret, err = service.SealCustomerPassword(target, "fixture-password")
	require.NoError(t, err)
	require.NoError(t, db.Create(&target).Error)
	id := common.GetUUID()
	secret, err := service.SealSupplySecret("resource/"+id, "fixture-key")
	require.NoError(t, err)
	r := model.SupplyResource{ID: id, SupplierID: owner, Target: target.ID, Secret: secret, CustomerGroup: "default", Revision: 2, Applied: 1, SupplyConfig: model.SupplyConfig{Name: "sample", Type: 1, Models: "demo"}}
	require.NoError(t, db.Create(&r).Error)
	t.Cleanup(func() {
		var ids []string
		require.NoError(t, db.Model(&model.SupplyAssignment{}).Where("resource_id = ?", id).Pluck("id", &ids).Error)
		require.NoError(t, db.Where("assignment_id IN ?", ids).Delete(&model.SupplyWindow{}).Error)
		require.NoError(t, db.Where("resource_id = ?", id).Delete(&model.SupplyAssignment{}).Error)
		require.NoError(t, db.Where("resource_id = ?", id).Delete(&model.SupplyEvent{}).Error)
		require.NoError(t, db.Delete(&r).Error)
		require.NoError(t, db.Delete(&target).Error)
	})
	require.NoError(t, service.ReconcileSupply(t.Context(), id))
	require.NoError(t, service.ReconcileSupply(t.Context(), id))
	assert.EqualValues(t, 1, creates.Load())
	visible.Store(true)
	require.NoError(t, service.ReconcileSupply(t.Context(), id))
	require.NoError(t, db.First(&r, "id = ?", id).Error)
	assert.Equal(t, r.Revision, r.Applied)
	var assignment model.SupplyAssignment
	require.NoError(t, db.First(&assignment, "id = ?", r.ActiveID).Error)
	assert.Equal(t, 53, assignment.ChannelID)
	window, err := service.CollectSupplyWindow(t.Context(), assignment, day)
	require.NoError(t, err)
	var rows []model.SupplyConsumption
	require.NoError(t, common.UnmarshalJsonStr(window.Content, &rows))
	quota := int64(4)
	assert.Equal(t, []model.SupplyConsumption{{Records: 2, InputTokens: 12, OutputTokens: 3, Quota: &quota, CacheReadTokens: new(int64)}}, rows)
	require.NoError(t, db.Model(&target).Update("usage_api", "channel_logs").Error)
	_, err = service.CollectSupplyWindow(t.Context(), assignment, day-86400)
	require.Error(t, err)
	window, err = service.CollectSupplyWindow(t.Context(), assignment, day-2*86400)
	require.NoError(t, err)
	require.NoError(t, common.UnmarshalJsonStr(window.Content, &rows))
	var byModel = make(map[string]model.SupplyConsumption, len(rows))
	for _, row := range rows {
		byModel[row.ModelName] = row
	}
	require.Contains(t, byModel, "recorded")
	require.NotNil(t, byModel["recorded"].CacheReadTokens)
	require.NotNil(t, byModel["recorded"].CacheWriteTokens)
	assert.EqualValues(t, 2, *byModel["recorded"].CacheReadTokens)
	assert.EqualValues(t, 12, *byModel["recorded"].CacheWriteTokens)
	require.Contains(t, byModel, "read-only")
	require.NotNil(t, byModel["read-only"].CacheReadTokens)
	assert.Zero(t, *byModel["read-only"].CacheReadTokens)
	assert.Nil(t, byModel["read-only"].CacheWriteTokens)
	// If the account temporarily loses sight of a channel, reassignment must wait.
	visible.Store(false)
	require.NoError(t, db.Model(&r).Updates(map[string]any{"target": "", "revision": 3}).Error)
	require.NoError(t, service.ReconcileSupply(t.Context(), id))
	require.NoError(t, db.First(&r, "id = ?", id).Error)
	assert.Equal(t, assignment.ID, r.ActiveID)
	assert.Less(t, r.Applied, r.Revision)
	assert.EqualValues(t, 1, creates.Load())
}

func TestSupplyDownstreamIdentityRequiresSensitiveChannelAccess(t *testing.T) {
	owner := setupChannelCreateTest(t)
	db := model.DB
	require.NoError(t, db.AutoMigrate(&model.CasbinRule{}, &model.AuthzRole{}, &model.SupplyResource{}, &model.SupplyTarget{}, &model.SupplyAssignment{}, &model.SupplyEvent{}, &model.SupplyWindow{}))
	common.IsMasterNode = true
	require.NoError(t, authz.Init(db))
	common.IsMasterNode = false
	supplier, manager := owner+1000000, owner+1000001
	for _, id := range []int{supplier, manager} {
		require.NoError(t, authz.SetUserPermissions(id, authz.PermissionsMap{authz.ResourceChannel: {
			authz.ActionRead: true, authz.ActionWrite: true, authz.ActionSensitiveWrite: id == manager,
		}}))
	}
	day := (common.GetTimestamp()/86400 - 1) * 86400
	target := model.SupplyTarget{ID: common.GetUUID(), Name: "private-downstream-name", URL: "https://private-downstream.invalid", LoginName: "private-downstream-login", Mode: model.SupplyTargetAccount}
	r := model.SupplyResource{ID: common.GetUUID(), SupplyConfig: model.SupplyConfig{Name: "Visible supply"}, SupplierID: supplier, Target: target.ID, ActiveID: common.GetUUID(), UserID: 9017, CustomerGroup: "private-group", Revision: 2, Applied: 2}
	a := model.SupplyAssignment{ID: r.ActiveID, ResourceID: r.ID, Target: target.ID, UserID: r.UserID, ChannelID: 8017, Started: day}
	provider := model.SupplyAssignment{ID: common.GetUUID(), ResourceID: r.ID, Target: target.ID, KeyRef: new(string), Ended: day, ProviderQuota: new(int64)}
	*provider.KeyRef = common.GetUUID()
	*provider.ProviderQuota = 15
	for _, row := range []any{&target, &r, &a, &provider,
		&model.SupplyEvent{ResourceID: r.ID, Target: target.ID, UserID: r.UserID, Action: "applied", Revision: 2},
		&model.SupplyWindow{AssignmentID: a.ID, Day: day, Collected: day + 86400, Content: `[{"model":"demo","records":3}]`},
	} {
		require.NoError(t, db.Create(row).Error)
	}
	call := func(user int, handler gin.HandlerFunc, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Set("id", user)
		c.Set("role", common.RoleAdminUser)
		c.Params = gin.Params{{Key: "id", Value: r.ID}}
		c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
		handler(c)
		return rec
	}
	usagePath := fmt.Sprintf("/?from=%d&to=%d", day, day)
	for _, endpoint := range []struct {
		handler      gin.HandlerFunc
		path, metric string
	}{
		{ListSupplyResources, "/", `"assigned":true`},
		{SupplyHistory, "/", `"action":"applied"`},
		{SupplyUsage, usagePath, `"records":3`},
		{DirectProviderUsage, "/", `"total_used":15`},
	} {
		limited := call(supplier, endpoint.handler, "GET", endpoint.path, "")
		assert.Equal(t, http.StatusOK, limited.Code)
		for _, private := range []string{target.ID, target.URL, target.Name, target.LoginName, r.CustomerGroup, "9017", "8017", a.ID} {
			assert.NotContains(t, limited.Body.String(), private)
		}
		assert.Contains(t, limited.Body.String(), endpoint.metric)
		full := call(manager, endpoint.handler, "GET", endpoint.path, "")
		assert.Contains(t, full.Body.String(), target.ID)
	}
	assert.Equal(t, http.StatusForbidden, call(supplier, SupplyTargets, "GET", "/", "").Code)
	assert.Contains(t, call(manager, SupplyTargets, "GET", "/", "").Body.String(), target.URL)
	assert.Equal(t, http.StatusForbidden, call(supplier, SupplyUsage, "GET", usagePath+"&target="+target.ID, "").Code)
	assert.Equal(t, http.StatusForbidden, call(supplier, SupplyUsage, "GET", usagePath+"&user_id=9017", "").Code)
	assert.Contains(t, call(manager, SupplyUsage, "GET", usagePath+"&target="+target.ID, "").Body.String(), `"records":3`)
	own := call(supplier, MySupplyUsage, "GET", usagePath, "")
	assert.Equal(t, http.StatusOK, own.Code)
	assert.Contains(t, own.Body.String(), `"channel":"Visible supply"`)
	assert.Contains(t, own.Body.String(), `"records":3`)
	for _, private := range []string{target.ID, target.Name, `"target"`, `"user_id"`, `"channel_id"`} {
		assert.NotContains(t, own.Body.String(), private)
	}
	others := call(manager, MySupplyUsage, "GET", usagePath, "")
	assert.Contains(t, others.Body.String(), `"rows":[]`)
	assert.Equal(t, http.StatusForbidden, call(supplier, MySupplyUsage, "GET", usagePath+"&target="+target.ID, "").Code)
	assert.Equal(t, http.StatusForbidden, call(supplier, AssignSupply, "PUT", "/", `{"revision":2}`).Code)
}
