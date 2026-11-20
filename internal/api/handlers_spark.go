package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api/middleware"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/spark"
)

// createSparkClusterRequest is the body of POST /spark/clusters.
type createSparkClusterRequest struct {
	Name           string   `json:"name"`
	SparkVersion   string   `json:"spark_version"`
	SubnetID       string   `json:"subnet_id"`
	MasterType     string   `json:"master_type"`
	WorkerType     string   `json:"worker_type"`
	WorkerCount    int      `json:"worker_count"`
	ObjectStorage  bool     `json:"object_storage"`
	Buckets        []string `json:"buckets"`
	EventLogBucket string   `json:"event_log_bucket"`
}

// sparkClientConfigResponse is the answer to POST
// /spark/clusters/{id}/client-config. The file carries the cluster's secret
// and S3 key, and is stored nowhere on the way (09-spark.md section 5).
type sparkClientConfigResponse struct {
	ClientConfig string `json:"client_config"`
	Filename     string `json:"filename"`
}

// sparkOff answers every /spark route on a node with the module off.
func (h *Handler) sparkOff(c *gin.Context) {
	WriteError(c, h.logger, fmt.Errorf("%w: the Spark module is off on this node; "+
		"set KUMO_SPARK=on (docs/design/09-spark.md)", service.ErrUnavailable))
	c.Abort()
}

// listSparkClusters answers GET /spark/clusters.
func (h *Handler) listSparkClusters(c *gin.Context) {
	in := spark.ListInput{Query: c.Query("q"), State: spark.State(c.Query("state")), PageToken: c.Query("page_token")}
	if raw := c.Query("page_size"); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil {
			WriteError(c, h.logger, Invalid("page_size is not acceptable",
				FieldError{Field: "page_size", Reason: "must be a number"}))
			return
		}
		in.PageSize = size
	}
	page, err := h.sparkClusters.List(c.Request.Context(), in)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

// createSparkCluster answers POST /spark/clusters: 202 with the cluster
// creating and a Location pointing at the task.
func (h *Handler) createSparkCluster(c *gin.Context) {
	var req createSparkClusterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with at least a name"}))
		return
	}
	created, err := h.sparkClusters.Create(c.Request.Context(), spark.CreateInput{
		Name: req.Name, SparkVersion: req.SparkVersion, SubnetID: req.SubnetID,
		MasterType: req.MasterType, WorkerType: req.WorkerType, WorkerCount: req.WorkerCount,
		ObjectStorage: req.ObjectStorage, Buckets: req.Buckets, EventLogBucket: req.EventLogBucket,
		CreatedBy: middleware.PrincipalFrom(c).UserID,
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

// getSparkCluster answers GET /spark/clusters/{id}.
func (h *Handler) getSparkCluster(c *gin.Context) {
	found, err := h.sparkClusters.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.JSON(http.StatusOK, found)
}

// deleteSparkCluster answers DELETE /spark/clusters/{id}: 202 and a task.
func (h *Handler) deleteSparkCluster(c *gin.Context) {
	queued, err := h.sparkClusters.Delete(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	if queued.TaskID != nil {
		c.Header("Location", "/api/v1/tasks/"+*queued.TaskID)
	}
	c.JSON(http.StatusAccepted, queued)
}

// sparkClientConfig answers POST /spark/clusters/{id}/client-config with the
// cluster's client spark-defaults.conf. A POST, like a kubeconfig, and never
// cached: the answer carries the cluster's secret and S3 key.
func (h *Handler) sparkClientConfig(c *gin.Context) {
	conf, err := h.sparkClusters.ClientConfig(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, sparkClientConfigResponse{
		ClientConfig: conf.Value(), Filename: "kumo-" + c.Param("id") + ".conf",
	})
}
