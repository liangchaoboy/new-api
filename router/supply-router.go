package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
)

func registerSupplyRoutes(api *gin.RouterGroup) {
	group := api.Group("/supply", middleware.DisableCache(), middleware.UserAuth())
	group.GET("/resources", controller.ListSupplyResources)
	group.POST("/resources", controller.SaveSupplyResource)
	group.PUT("/resources/:id", controller.SaveSupplyResource)
	group.DELETE("/resources/:id", controller.WithdrawSupply)
	group.POST("/resources/:id/withdraw", controller.WithdrawSupply)
	group.GET("/resources/:id/history", controller.SupplyHistory)
	group.POST("/resources/:id/revocation", controller.ConfirmSupplyRevocation)
	group.GET("/resources/:id/provider-usage", controller.DirectProviderUsage)
	group.GET("/resources/:id/usage", controller.SupplyUsage)
	api.POST("/supply/resources/:id/delivery", middleware.RootAuth(), middleware.CriticalRateLimit(), middleware.DisableCache(), controller.ExportSupplyDelivery)
	admin := api.Group("/supply", middleware.DisableCache(), middleware.AdminAuth())
	handlePermissionRoute(admin, "POST", "/resources/:id/retry", authz.ChannelWrite, controller.RetrySupply)
	handlePermissionRoute(admin, "PUT", "/resources/:id/assignment", authz.ChannelWrite, controller.AssignSupply)
	handlePermissionRoute(admin, "GET", "/targets", authz.ChannelRead, controller.SupplyTargets)
	handlePermissionRoute(admin, "POST", "/targets", authz.ChannelSensitiveWrite, middleware.UserCriticalRateLimit("supply-target-verification"), controller.SaveSupplyTarget)
	handlePermissionRoute(admin, "PUT", "/targets/:id", authz.ChannelSensitiveWrite, middleware.UserCriticalRateLimit("supply-target-verification"), controller.SaveSupplyTarget)
	receiver := api.Group("/supply-receiver", middleware.DisableCache(), middleware.AdminAuth())
	handlePermissionRoute(receiver, "GET", "/identity", authz.SupplyReceive, controller.SupplyReceiverIdentity)
	handlePermissionRoute(receiver, "PUT", "/:id", authz.SupplyReceive, controller.ReceiveSupply)
	handlePermissionRoute(receiver, "GET", "/:id/usage", authz.SupplyReceive, controller.ReadSupplyWindow)
}
