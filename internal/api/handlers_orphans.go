package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/service/instance"
)

// orphansResponse is the envelope of GET /system/orphans, a list in the same
// `items` shape every collection in api/openapi.yaml uses. There is no
// pagination: the list is what one reconcile pass observed, and a node with
// enough orphans to page is a node with a problem no page token helps with.
type orphansResponse struct {
	Items []instance.Orphan `json:"items"`
}

// systemOrphans answers GET /api/v1/system/orphans: the domains the reconciler
// found running under an instance id that no row claims
// (docs/design/00-architecture.md §4.3).
//
// It is admin-only. An orphan names a guest the control plane does not manage,
// which is host-level detail rather than something a viewer of the console needs,
// and the report is the thing an operator acts on by hand.
//
// The list is served from the last pass rather than probed on request, so the
// endpoint answers at the speed of a map read and cannot be used to hammer
// libvirt. An empty list means the last pass found nothing, which is also what a
// node that has never completed a pass reports.
func (h *Handler) systemOrphans(c *gin.Context) {
	c.JSON(http.StatusOK, orphansResponse{Items: h.instances.Orphans()})
}
