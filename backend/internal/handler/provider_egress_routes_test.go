package handler

import (
	"testing"

	"yingce/backend/internal/service"

	"github.com/gin-gonic/gin"
)

func TestProviderEgressRoutesAreRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/api")
	RegisterAdminRoutes(group, &service.Service{})
	wanted := map[string]bool{
		"GET /api/admin/settings/provider-egress":             false,
		"PATCH /api/admin/settings/provider-egress":           false,
		"POST /api/admin/settings/provider-egress/test":       false,
		"DELETE /api/admin/settings/provider-egress/mappings": false,
	}
	for _, route := range router.Routes() {
		key := route.Method + " " + route.Path
		if _, exists := wanted[key]; exists {
			wanted[key] = true
		}
	}
	for route, found := range wanted {
		if !found {
			t.Errorf("route %s is not registered", route)
		}
	}
}
