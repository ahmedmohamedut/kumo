package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api/middleware"
	"github.com/ahmedmohamedut/kumo/internal/service/vpc"
)

// createVPCRequest is the body of POST /vpcs.
type createVPCRequest struct {
	Name string            `json:"name"`
	CIDR string            `json:"cidr"`
	Mode vpc.Mode          `json:"mode"`
	Tags map[string]string `json:"tags"`
}

// updateVPCRequest is the body of PATCH /vpcs/{id}: tags only
// (docs/design/01-api.md §VPCs).
type updateVPCRequest struct {
	Tags map[string]string `json:"tags"`
}

// listVPCs answers GET /vpcs with one page of VPCs.
func (h *Handler) listVPCs(c *gin.Context) {
	input := vpc.ListInput{
		Query:     c.Query("q"),
		Sort:      vpc.SortOrder(c.Query("sort")),
		PageToken: c.Query("page_token"),
	}

	if raw := c.Query("page_size"); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < vpc.MinPageSize || size > vpc.MaxPageSize {
			WriteError(c, h.logger, Invalid("page_size is not acceptable",
				FieldError{Field: "page_size", Reason: fmt.Sprintf(
					"must be between %d and %d", vpc.MinPageSize, vpc.MaxPageSize)}))
			return
		}
		input.PageSize = size
	}

	page, err := h.vpcs.List(c.Request.Context(), input)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, page)
}

// createVPC answers POST /vpcs. Defining and starting the network is long, so
// the response is 202 with the task that will do it and a Location pointing at
// the task (docs/design/01-api.md §VPCs).
func (h *Handler) createVPC(c *gin.Context) {
	var req createVPCRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with a name and a cidr"}))
		return
	}

	created, err := h.vpcs.Create(c.Request.Context(), vpc.CreateInput{
		Name:      req.Name,
		CIDR:      req.CIDR,
		Mode:      req.Mode,
		Tags:      req.Tags,
		CreatedBy: middleware.PrincipalFrom(c).UserID,
	})
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.Header("Location", "/api/v1/tasks/"+created.TaskID)
	c.JSON(http.StatusAccepted, created)
}

// getVPC answers GET /vpcs/{id} with the counts of what lives inside.
func (h *Handler) getVPC(c *gin.Context) {
	detail, err := h.vpcs.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, detail)
}

// updateVPC answers PATCH /vpcs/{id}, which replaces the tag set. The name,
// the CIDR and the mode do not move.
func (h *Handler) updateVPC(c *gin.Context) {
	var req updateVPCRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with tags"}))
		return
	}
	if req.Tags == nil {
		WriteError(c, h.logger, Invalid("the request updates nothing",
			FieldError{Field: "tags", Reason: "must be present; pass {} to remove all tags"}))
		return
	}

	updated, err := h.vpcs.Update(c.Request.Context(), c.Param("id"), req.Tags)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, updated)
}

// deleteVPC answers DELETE /vpcs/{id}. The network teardown is long, so the
// response is 202 with the task that will do it and a Location pointing at it.
func (h *Handler) deleteVPC(c *gin.Context) {
	deleted, err := h.vpcs.Delete(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.Header("Location", "/api/v1/tasks/"+deleted.TaskID)
	c.JSON(http.StatusAccepted, deleted)
}
