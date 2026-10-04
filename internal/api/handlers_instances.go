package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api/middleware"
	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
)

// launchInstanceRequest is the body of POST /instances. Count is accepted
// because the contract documents it, but this release launches one instance per
// request and the service refuses anything else.
type launchInstanceRequest struct {
	Name         string            `json:"name"`
	ImageID      string            `json:"image_id"`
	InstanceType string            `json:"instance_type"`
	KeyPairID    string            `json:"key_pair_id"`
	RootDiskGB   int               `json:"root_disk_gb"`
	UserData     string            `json:"user_data"`
	Count        int               `json:"count"`
	Tags         map[string]string `json:"tags"`
}

// listInstances answers GET /instances.
func (h *Handler) listInstances(c *gin.Context) {
	input := instance.ListInput{
		Query:     c.Query("q"),
		State:     instance.State(c.Query("state")),
		Sort:      instance.SortOrder(c.Query("sort")),
		PageToken: c.Query("page_token"),
	}

	if raw := c.Query("page_size"); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < instance.MinPageSize || size > instance.MaxPageSize {
			WriteError(c, h.logger, Invalid("page_size is not acceptable",
				FieldError{Field: "page_size", Reason: fmt.Sprintf(
					"must be between %d and %d", instance.MinPageSize, instance.MaxPageSize)}))
			return
		}
		input.PageSize = size
	}

	page, err := h.instances.List(c.Request.Context(), input)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, page)
}

// launchInstance answers POST /instances. Everything after this response is a
// task: the root disk, the seed ISO, the domain and the address are all slower
// than an HTTP request, so the answer is 202 with the instance in pending and a
// Location pointing at the task (docs/design/01-api.md §Instances).
func (h *Handler) launchInstance(c *gin.Context) {
	var req launchInstanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with a name, an image_id and an instance_type"}))
		return
	}

	launched, err := h.instances.Launch(c.Request.Context(), instance.LaunchInput{
		Name:         req.Name,
		ImageID:      req.ImageID,
		InstanceType: req.InstanceType,
		KeyPairID:    req.KeyPairID,
		RootDiskGB:   req.RootDiskGB,
		UserData:     req.UserData,
		Count:        req.Count,
		Tags:         req.Tags,
		CreatedBy:    middleware.PrincipalFrom(c).UserID,
	})
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	if launched.TaskID != nil {
		c.Header("Location", "/api/v1/tasks/"+*launched.TaskID)
	}
	c.JSON(http.StatusAccepted, launched)
}

// getInstance answers GET /instances/{id}.
func (h *Handler) getInstance(c *gin.Context) {
	found, err := h.instances.Get(c.Request.Context(), c.Param("id"),
		middleware.PrincipalFrom(c).Role == auth.RoleAdmin)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, found)
}
