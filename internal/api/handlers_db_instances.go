package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api/middleware"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/dbinstance"
)

// createDBInstanceRequest is the body of POST /rds/db-instances.
//
// AdminPassword is the one field that goes no further than the service: a
// verifier is derived from it and the password itself is never stored, logged
// or put in a task payload (docs/design/07-databases.md R8). port is not
// accepted in this version.
type createDBInstanceRequest struct {
	Identifier    string `json:"identifier"`
	Engine        string `json:"engine"`
	EngineVersion string `json:"engine_version"`
	InstanceType  string `json:"instance_type"`
	StorageGB     int    `json:"storage_gb"`
	SubnetID      string `json:"subnet_id"`
	AdminUsername string `json:"admin_username"`
	AdminPassword string `json:"admin_password"`
	DatabaseName  string `json:"database_name"`
}

// databasesOff answers every /rds route on a node with the module turned off,
// so a client learns that rather than that the path is wrong.
func (h *Handler) databasesOff(c *gin.Context) {
	WriteError(c, h.logger, fmt.Errorf("%w: the databases module is off on this node; "+
		"set KUMO_DATABASES=on (docs/design/07-databases.md)", service.ErrUnavailable))
	c.Abort()
}

// listDBInstances answers GET /rds/db-instances.
func (h *Handler) listDBInstances(c *gin.Context) {
	input := dbinstance.ListInput{
		Query:     c.Query("q"),
		State:     dbinstance.State(c.Query("state")),
		Sort:      dbinstance.SortOrder(c.Query("sort")),
		PageToken: c.Query("page_token"),
	}

	if raw := c.Query("page_size"); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < dbinstance.MinPageSize || size > dbinstance.MaxPageSize {
			WriteError(c, h.logger, Invalid("page_size is not acceptable",
				FieldError{Field: "page_size", Reason: fmt.Sprintf(
					"must be between %d and %d", dbinstance.MinPageSize, dbinstance.MaxPageSize)}))
			return
		}
		input.PageSize = size
	}

	page, err := h.dbInstances.List(c.Request.Context(), input)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, page)
}

// createDBInstance answers POST /rds/db-instances. Everything after this
// response is a task: a data disk, a guest and a first boot that creates a
// cluster are all slower than an HTTP request, so the answer is 202 with the
// DB instance creating and a Location pointing at the task.
func (h *Handler) createDBInstance(c *gin.Context) {
	var req createDBInstanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with an identifier, " +
				"an instance_type, an admin_username, an admin_password and a database_name"}))
		return
	}

	created, err := h.dbInstances.Create(c.Request.Context(), dbinstance.CreateInput{
		Identifier:    req.Identifier,
		Engine:        dbinstance.Engine(req.Engine),
		EngineVersion: req.EngineVersion,
		InstanceType:  req.InstanceType,
		StorageGB:     req.StorageGB,
		SubnetID:      req.SubnetID,
		AdminUsername: req.AdminUsername,
		AdminPassword: req.AdminPassword,
		DatabaseName:  req.DatabaseName,
		CreatedBy:     middleware.PrincipalFrom(c).UserID,
	})
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	if created.TaskID != nil {
		c.Header("Location", "/api/v1/tasks/"+*created.TaskID)
	}
	c.JSON(http.StatusAccepted, created)
}

// getDBInstance answers GET /rds/db-instances/{id}.
func (h *Handler) getDBInstance(c *gin.Context) {
	found, err := h.dbInstances.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.JSON(http.StatusOK, found)
}

// stopDBInstance answers POST /rds/db-instances/{id}/stop.
func (h *Handler) stopDBInstance(c *gin.Context) {
	h.queueDBInstanceAction(c, h.dbInstances.Stop)
}

// startDBInstance answers POST /rds/db-instances/{id}/start.
func (h *Handler) startDBInstance(c *gin.Context) {
	h.queueDBInstanceAction(c, h.dbInstances.Start)
}

// rebootDBInstance answers POST /rds/db-instances/{id}/reboot.
func (h *Handler) rebootDBInstance(c *gin.Context) {
	h.queueDBInstanceAction(c, h.dbInstances.Reboot)
}

// deleteDBInstance answers DELETE /rds/db-instances/{id}. It is 202 and a
// task, not 204: the guest, the data disk and both rows take longer than a
// request, and the data is gone when it finishes.
func (h *Handler) deleteDBInstance(c *gin.Context) {
	h.queueDBInstanceAction(c, h.dbInstances.Delete)
}

// queueDBInstanceAction is the shared half of the lifecycle endpoints: every
// one of them answers 202 with the DB instance and the task that carries the
// work (docs/design/07-databases.md section 8).
func (h *Handler) queueDBInstanceAction(
	c *gin.Context,
	action func(ctx context.Context, dbInstanceID string) (dbinstance.DBInstance, error),
) {
	queued, err := action(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	if queued.TaskID != nil {
		c.Header("Location", "/api/v1/tasks/"+*queued.TaskID)
	}
	c.JSON(http.StatusAccepted, queued)
}
