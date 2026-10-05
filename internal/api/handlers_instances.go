package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api/middleware"
	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/service"
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

// updateInstanceRequest is the body of PATCH /instances/{id}. A field the body
// does not carry is left alone, and an empty tag map means every tag goes: the
// two are told apart by the pointer and the nil map, not by the handler.
type updateInstanceRequest struct {
	Name *string           `json:"name"`
	Tags map[string]string `json:"tags"`
}

// updateInstance answers PATCH /instances/{id}. The only editable columns are the
// name and the tags: everything else about an instance is either an input of its
// launch or runtime data libvirt owns, and a PATCH that changed one of them would
// be claiming something the host is not doing.
func (h *Handler) updateInstance(c *gin.Context) {
	var req updateInstanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with a name, tags or both"}))
		return
	}

	updated, err := h.instances.Update(c.Request.Context(), c.Param("id"), instance.UpdateInput{
		Name: req.Name,
		Tags: req.Tags,
	}, middleware.PrincipalFrom(c).Role == auth.RoleAdmin)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, updated)
}

// startInstance answers POST /instances/{id}/start.
func (h *Handler) startInstance(c *gin.Context) {
	h.queueLifecycle(c, h.instances.Start)
}

// stopInstance answers POST /instances/{id}/stop. ?force=true overrides the guest
// instead of asking it.
func (h *Handler) stopInstance(c *gin.Context) {
	force, ok := forceFlag(c, h)
	if !ok {
		return
	}
	h.queueLifecycle(c, func(ctx context.Context, id string) (instance.Instance, error) {
		return h.instances.Stop(ctx, id, force)
	})
}

// rebootInstance answers POST /instances/{id}/reboot. ?force=true resets the
// domain instead of asking the guest.
func (h *Handler) rebootInstance(c *gin.Context) {
	force, ok := forceFlag(c, h)
	if !ok {
		return
	}
	h.queueLifecycle(c, func(ctx context.Context, id string) (instance.Instance, error) {
		return h.instances.Reboot(ctx, id, force)
	})
}

// terminateInstance answers DELETE /instances/{id}.
func (h *Handler) terminateInstance(c *gin.Context) {
	h.queueLifecycle(c, h.instances.Terminate)
}

// queueLifecycle is the body of the four lifecycle endpoints.
//
// Every one of them is the same shape: the service validates the request, writes
// the state the action implies and a task, and the worker does the work. So every
// one of them answers 202 with the instance and a Location pointing at the task,
// including the refusals that never reach the worker: a 409 comes back from the
// service, not from a task the operator has to go and look at.
func (h *Handler) queueLifecycle(
	c *gin.Context,
	queue func(context.Context, string) (instance.Instance, error),
) {
	queued, err := queue(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	if queued.TaskID != nil {
		c.Header("Location", "/api/v1/tasks/"+*queued.TaskID)
	}
	c.JSON(http.StatusAccepted, queued)
}

// forceFlag reads ?force=.
//
// A value that is not a boolean is a 400 rather than a silent false: a stop that
// was meant to be forced must not come back as a graceful request the guest can
// refuse, because the operator would then have to watch a 90 second wait for a
// shutdown that was never going to happen.
func forceFlag(c *gin.Context, h *Handler) (bool, bool) {
	raw := c.Query("force")
	if raw == "" {
		return false, true
	}
	force, err := strconv.ParseBool(raw)
	if err != nil {
		WriteError(c, h.logger, Invalid("force is not acceptable",
			FieldError{Field: "force", Reason: "must be true or false"}))
		return false, false
	}
	return force, true
}

// instanceConsoleOutput answers GET /instances/{id}/console-output.
func (h *Handler) instanceConsoleOutput(c *gin.Context) {
	tail := instance.DefaultConsoleTailKiB
	if raw := c.Query("tail_kib"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < instance.MinConsoleTailKiB || parsed > instance.MaxConsoleTailKiB {
			WriteError(c, h.logger, Invalid("tail_kib is not acceptable",
				FieldError{Field: "tail_kib", Reason: fmt.Sprintf(
					"must be between %d and %d", instance.MinConsoleTailKiB, instance.MaxConsoleTailKiB)}))
			return
		}
		tail = parsed
	}

	out, err := h.instances.ConsoleOutput(c.Request.Context(), c.Param("id"), tail)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, out)
}

// openInstanceConsole answers GET /instances/{id}/console with 501 until the VNC
// proxy of M5 exists.
//
// The route is registered so a client that follows the contract gets an honest
// answer about this release rather than a 404 that reads like a missing endpoint.
// The instance is looked up first, because "not built yet" and "no such instance"
// are different answers and a client that cannot tell them apart cannot recover.
// The code in the envelope is `unavailable` because the error code list of
// api/openapi.yaml has no member for "not built yet", and the status carries that
// meaning on its own.
func (h *Handler) openInstanceConsole(c *gin.Context) {
	if _, err := h.instances.Get(c.Request.Context(), c.Param("id"), false); err != nil {
		WriteError(c, h.logger, err)
		return
	}

	WriteError(c, h.logger, &statusError{
		status: http.StatusNotImplemented,
		code:   CodeUnavailable,
		err: fmt.Errorf("%s %s: the VNC console proxy is scheduled for M5: %w",
			c.Request.Method, c.Request.URL.Path, service.ErrUnavailable),
	})
}
