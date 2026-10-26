package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
)

// instanceTypesResponse is the body of GET /api/v1/instance-types.
type instanceTypesResponse struct {
	Items []instancetype.Type `json:"items"`
}

// createInstanceTypeRequest is the body of POST /instance-types.
type createInstanceTypeRequest struct {
	ID          string `json:"id"`
	VCPUs       int    `json:"vcpus"`
	MemoryMiB   int    `json:"memory_mib"`
	Description string `json:"description"`
}

// updateInstanceTypeRequest is the body of PATCH /instance-types/{id}. A nil
// field is left alone; the id is immutable, because instances reference it.
type updateInstanceTypeRequest struct {
	VCPUs       *int    `json:"vcpus"`
	MemoryMiB   *int    `json:"memory_mib"`
	Description *string `json:"description"`
}

// listInstanceTypes answers GET /api/v1/instance-types. The types are
// readable by both roles: a viewer picks one in the launch form, they just may
// not submit it.
func (h *Handler) listInstanceTypes(c *gin.Context) {
	types, err := h.instanceTypes.List(c.Request.Context())
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, instanceTypesResponse{Items: types})
}

// getInstanceType answers GET /instance-types/{id} with the instances that
// reference the type, which is what a delete would be refused for.
func (h *Handler) getInstanceType(c *gin.Context) {
	detail, err := h.instanceTypes.Detail(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, detail)
}

// createInstanceType answers POST /instance-types.
func (h *Handler) createInstanceType(c *gin.Context) {
	var req createInstanceTypeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with an id, vcpus and memory_mib"}))
		return
	}

	created, err := h.instanceTypes.Create(c.Request.Context(), instancetype.CreateInput{
		ID:          req.ID,
		VCPUs:       req.VCPUs,
		MemoryMiB:   req.MemoryMiB,
		Description: req.Description,
	})
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.Header("Location", "/api/v1/instance-types/"+created.ID)
	c.JSON(http.StatusCreated, created)
}

// updateInstanceType answers PATCH /instance-types/{id}. Instances already
// launched keep their size; the change applies to later launches.
func (h *Handler) updateInstanceType(c *gin.Context) {
	var req updateInstanceTypeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with vcpus, memory_mib or description"}))
		return
	}
	if req.VCPUs == nil && req.MemoryMiB == nil && req.Description == nil {
		WriteError(c, h.logger, Invalid("the request updates nothing",
			FieldError{Field: "body", Reason: "must set vcpus, memory_mib or description"}))
		return
	}

	updated, err := h.instanceTypes.Update(c.Request.Context(), c.Param("id"), instancetype.UpdateInput{
		VCPUs:       req.VCPUs,
		MemoryMiB:   req.MemoryMiB,
		Description: req.Description,
	})
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, updated)
}

// deleteInstanceType answers DELETE /instance-types/{id}; 409 while any
// instance references the type.
func (h *Handler) deleteInstanceType(c *gin.Context) {
	if err := h.instanceTypes.Delete(c.Request.Context(), c.Param("id")); err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.Status(http.StatusNoContent)
}
