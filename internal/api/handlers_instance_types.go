package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// instanceTypesResponse is the body of GET /api/v1/instance-types.
type instanceTypesResponse struct {
	Items []instanceTypeItem `json:"items"`
}

// instanceTypeItem is one preset on the wire.
type instanceTypeItem struct {
	ID          string `json:"id"`
	VCPUs       int    `json:"vcpus"`
	MemoryMiB   int    `json:"memory_mib"`
	Description string `json:"description"`
}

// listInstanceTypes answers GET /api/v1/instance-types. The presets are
// readable by both roles: a viewer picks one in the launch form, they just may
// not submit it.
func (h *Handler) listInstanceTypes(c *gin.Context) {
	types, err := h.instanceTypes.List(c.Request.Context())
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	items := make([]instanceTypeItem, 0, len(types))
	for _, t := range types {
		items = append(items, instanceTypeItem{
			ID:          t.ID,
			VCPUs:       t.VCPUs,
			MemoryMiB:   t.MemoryMiB,
			Description: t.Description,
		})
	}

	c.JSON(http.StatusOK, instanceTypesResponse{Items: items})
}
