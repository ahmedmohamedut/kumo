package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api/middleware"
	"github.com/ahmedmohamedut/kumo/internal/service/subnet"
)

// createSubnetRequest is the body of POST /subnets.
type createSubnetRequest struct {
	VPCID string            `json:"vpc_id"`
	Name  string            `json:"name"`
	CIDR  string            `json:"cidr"`
	Tags  map[string]string `json:"tags"`
}

// updateSubnetRequest is the body of PATCH /subnets/{id}: tags only
// (docs/design/01-api.md §Subnets).
type updateSubnetRequest struct {
	Tags map[string]string `json:"tags"`
}

// listSubnets answers GET /subnets with one page of subnets. The optional
// vpc_id filter keeps a page to one VPC, which is what the VPC detail page
// asks for.
func (h *Handler) listSubnets(c *gin.Context) {
	input := subnet.ListInput{
		Query:     c.Query("q"),
		VpcID:     c.Query("vpc_id"),
		Sort:      subnet.SortOrder(c.Query("sort")),
		PageToken: c.Query("page_token"),
	}

	if raw := c.Query("page_size"); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < subnet.MinPageSize || size > subnet.MaxPageSize {
			WriteError(c, h.logger, Invalid("page_size is not acceptable",
				FieldError{Field: "page_size", Reason: fmt.Sprintf(
					"must be between %d and %d", subnet.MinPageSize, subnet.MaxPageSize)}))
			return
		}
		input.PageSize = size
	}

	page, err := h.subnets.List(c.Request.Context(), input)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, page)
}

// createSubnet answers POST /subnets. Unlike a VPC, a subnet is only a row, so
// the response is a synchronous 201 (docs/design/01-api.md §Subnets).
func (h *Handler) createSubnet(c *gin.Context) {
	var req createSubnetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with a vpc_id, a name and a cidr"}))
		return
	}

	created, err := h.subnets.Create(c.Request.Context(), subnet.CreateInput{
		VPCID:     req.VPCID,
		Name:      req.Name,
		CIDR:      req.CIDR,
		Tags:      req.Tags,
		CreatedBy: middleware.PrincipalFrom(c).UserID,
	})
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusCreated, created)
}

// getSubnet answers GET /subnets/{id} with the counts of its addresses.
func (h *Handler) getSubnet(c *gin.Context) {
	detail, err := h.subnets.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, detail)
}

// updateSubnet answers PATCH /subnets/{id}, which replaces the tag set. The
// name, the CIDR and the parent VPC do not move.
func (h *Handler) updateSubnet(c *gin.Context) {
	var req updateSubnetRequest
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

	updated, err := h.subnets.Update(c.Request.Context(), c.Param("id"), req.Tags)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, updated)
}

// deleteSubnet answers DELETE /subnets/{id}. A subnet that still has network
// interfaces allocated from it is a 409.
func (h *Handler) deleteSubnet(c *gin.Context) {
	if err := h.subnets.Delete(c.Request.Context(), c.Param("id")); err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.Status(http.StatusNoContent)
}
