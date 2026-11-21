package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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
	issuer := setupChannelCreateTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.SupplyReceipt{}))
	id := common.GetUUID()
	t.Cleanup(func() { require.NoError(t, model.DB.Where("id = ?", id).Delete(&model.SupplyReceipt{}).Error) })
	cmd := model.SupplyCommand{UserID: issuer, Version: 1, Key: "mock-credential", Config: model.SupplyConfig{Name: "dedicated", Type: 1, Models: "gpt-4o"}}
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
	cmd.Version = 2
	cmd.Key = "rotated"
	updated, err := model.ReceiveSupply(id, issuer, cmd)
	require.NoError(t, err)
	assert.Equal(t, first.ChannelID, updated.ChannelID)
	day := common.GetTimestamp() / 86400 * 86400
	log := model.Log{UserId: issuer, ChannelId: first.ChannelID, Type: model.LogTypeConsume, ModelName: "gpt-4o", CreatedAt: day + 1, PromptTokens: 12, CompletionTokens: 4}
	require.NoError(t, model.LOG_DB.Create(&log).Error)
	t.Cleanup(func() { require.NoError(t, model.LOG_DB.Delete(&log).Error) })
	cmd.Version = 3
	cmd.Retire = true
	receipt, err := model.ReceiveSupply(id, issuer, cmd)
	require.NoError(t, err)
	require.True(t, receipt.Retired)
	bound, err := model.GetChannelById(first.ChannelID, true)
	require.NoError(t, err)
	assert.Empty(t, bound.Key)
	require.NoError(t, model.DB.Model(bound).Update("status", common.ChannelStatusEnabled).Error)
	_, err = bound.ActiveForUser(issuer)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	_, err = model.BatchDeleteChannels([]int{first.ChannelID})
	require.ErrorIs(t, err, model.ErrSupplyConflict)
	t.Cleanup(func() { require.NoError(t, model.DB.Where("id = ?", first.ChannelID).Delete(&model.Channel{}).Error) })
	cmd.Version = 4
	cmd.Retire = false
	_, err = model.ReceiveSupply(id, issuer, cmd)
	require.ErrorIs(t, err, model.ErrSupplyConflict)
	rows, err := model.SupplyDayUsage(receipt, day)
	require.NoError(t, err)
	assert.Equal(t, []model.SupplyConsumption{{ModelName: "gpt-4o", Records: 1, InputTokens: 12, OutputTokens: 4}}, rows)
}
