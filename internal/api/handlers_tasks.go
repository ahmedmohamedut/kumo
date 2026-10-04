package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/service/task"
)

// listTasks answers GET /tasks. ?resource_id=<id> narrows the page to the tasks
// of one image (docs/design/01-api.md §Tasks).
func (h *Handler) listTasks(c *gin.Context) {
	input := task.ListInput{
		ResourceID: c.Query("resource_id"),
		Sort:       task.SortOrder(c.Query("sort")),
		PageToken:  c.Query("page_token"),
	}

	if raw := c.Query("page_size"); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < task.MinPageSize || size > task.MaxPageSize {
			WriteError(c, h.logger, Invalid("page_size is not acceptable",
				FieldError{Field: "page_size", Reason: fmt.Sprintf(
					"must be between %d and %d", task.MinPageSize, task.MaxPageSize)}))
			return
		}
		input.PageSize = size
	}

	page, err := h.tasks.List(c.Request.Context(), input)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, page)
}

// getTask answers GET /tasks/{id}. This is what the Location header of an
// asynchronous image operation points at, so the console can poll it.
func (h *Handler) getTask(c *gin.Context) {
	found, err := h.tasks.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, found)
}
