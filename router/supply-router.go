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
	group.GET("/resources/:id/usage", controller.SupplyUsage)
	admin := api.Group("/supply", middleware.DisableCache(), middleware.AdminAuth())
	handlePermissionRoute(admin, "POST", "/resources/:id/retry", authz.ChannelWrite, controller.RetrySupply)
	handlePermissionRoute(admin, "PUT", "/resources/:id/assignment", authz.ChannelWrite, controller.AssignSupply)
	handlePermissionRoute(admin, "GET", "/targets", authz.ChannelRead, controller.SupplyTargets)
	handlePermissionRoute(admin, "POST", "/targets", authz.ChannelSensitiveWrite, controller.SaveSupplyTarget)
	handlePermissionRoute(admin, "PUT", "/targets/:id", authz.ChannelSensitiveWrite, controller.SaveSupplyTarget)
	receiver := api.Group("/supply-receiver", middleware.DisableCache(), middleware.AdminAuth())
	handlePermissionRoute(receiver, "GET", "/identity", authz.SupplyReceive, controller.SupplyReceiverIdentity)
	handlePermissionRoute(receiver, "PUT", "/:id", authz.SupplyReceive, controller.ReceiveSupply)
	handlePermissionRoute(receiver, "GET", "/:id/usage", authz.SupplyReceive, controller.ReadSupplyWindow)
}
