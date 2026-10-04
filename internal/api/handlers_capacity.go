package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// systemCapacity answers GET /api/v1/system/capacity: what the host has and
// what the instances hold. Both roles may read it, because the launch form
// shows it to whoever is looking at the console.
func (h *Handler) systemCapacity(c *gin.Context) {
	report, err := h.system.Capacity(c.Request.Context())
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, report)
}
