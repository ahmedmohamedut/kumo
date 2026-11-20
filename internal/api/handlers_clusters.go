package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api/middleware"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/kubernetes"
)

// createClusterRequest is the body of POST /eks/clusters.
type createClusterRequest struct {
	Name              string `json:"name"`
	KubernetesVersion string `json:"kubernetes_version"`
	SubnetID          string `json:"subnet_id"`
	ControlPlaneType  string `json:"control_plane_type"`
	NodeType          string `json:"node_type"`
	NodeCount         int    `json:"node_count"`
	NodeRootDiskGB    int    `json:"node_root_disk_gb"`
}

// kubeconfigResponse is the answer to POST /eks/clusters/{id}/kubeconfig. The
// file is returned once and stored nowhere (08-kubernetes.md section 5).
type kubeconfigResponse struct {
	Kubeconfig string    `json:"kubeconfig"`
	Group      string    `json:"group"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// kubernetesOff answers every /eks route on a node with the module off.
func (h *Handler) kubernetesOff(c *gin.Context) {
	WriteError(c, h.logger, fmt.Errorf("%w: the Kubernetes module is off on this node; "+
		"set KUMO_KUBERNETES=on (docs/design/08-kubernetes.md)", service.ErrUnavailable))
	c.Abort()
}

// listClusters answers GET /eks/clusters.
func (h *Handler) listClusters(c *gin.Context) {
	in := kubernetes.ListInput{
		Query: c.Query("q"), State: kubernetes.State(c.Query("state")), PageToken: c.Query("page_token"),
	}
	if raw := c.Query("page_size"); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil {
			WriteError(c, h.logger, Invalid("page_size is not acceptable",
				FieldError{Field: "page_size", Reason: "must be a number"}))
			return
		}
		in.PageSize = size
	}
	page, err := h.clusters.List(c.Request.Context(), in)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

// createCluster answers POST /eks/clusters: 202 with the cluster creating
// and a Location pointing at the task, because a cluster takes minutes.
func (h *Handler) createCluster(c *gin.Context) {
	var req createClusterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with at least a name"}))
		return
	}
	created, err := h.clusters.Create(c.Request.Context(), kubernetes.CreateInput{
		Name: req.Name, KubernetesVersion: req.KubernetesVersion, SubnetID: req.SubnetID,
		ControlPlaneType: req.ControlPlaneType, NodeType: req.NodeType,
		NodeCount: req.NodeCount, NodeRootDiskGB: req.NodeRootDiskGB,
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

// getCluster answers GET /eks/clusters/{id}.
func (h *Handler) getCluster(c *gin.Context) {
	found, err := h.clusters.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.JSON(http.StatusOK, found)
}

// listNodeGroups answers GET /eks/clusters/{id}/node-groups.
func (h *Handler) listNodeGroups(c *gin.Context) {
	found, err := h.clusters.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": found.NodeGroups})
}

// scaleNodeGroupRequest is the body of PATCH /eks/clusters/{id}/node-groups/{ng}.
type scaleNodeGroupRequest struct {
	DesiredSize *int `json:"desired_size"`
}

// scaleNodeGroup answers PATCH /eks/clusters/{id}/node-groups/{ng}: 202 and a
// k8s.scale task, or 200 with the cluster unchanged when the size already is
// the one asked for (08-kubernetes.md section 4).
func (h *Handler) scaleNodeGroup(c *gin.Context) {
	var req scaleNodeGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.DesiredSize == nil {
		WriteError(c, h.logger, Invalid("the request body is not acceptable",
			FieldError{Field: "desired_size", Reason: "is required"}))
		return
	}
	scaled, err := h.clusters.Scale(c.Request.Context(), c.Param("id"), c.Param("ng"), *req.DesiredSize)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	if scaled.TaskID == nil {
		c.JSON(http.StatusOK, scaled)
		return
	}
	c.Header("Location", "/api/v1/tasks/"+*scaled.TaskID)
	c.JSON(http.StatusAccepted, scaled)
}

// deleteCluster answers DELETE /eks/clusters/{id}: 202 and a task.
func (h *Handler) deleteCluster(c *gin.Context) {
	queued, err := h.clusters.Delete(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	if queued.TaskID != nil {
		c.Header("Location", "/api/v1/tasks/"+*queued.TaskID)
	}
	c.JSON(http.StatusAccepted, queued)
}

// mintKubeconfig answers POST /eks/clusters/{id}/kubeconfig with a fresh
// kubeconfig for the caller, in the group of their role (K10). A POST rather
// than a GET: every call signs a new certificate, and the response carries a
// private key that must not sit in a cache.
func (h *Handler) mintKubeconfig(c *gin.Context) {
	principal := middleware.PrincipalFrom(c)
	username := principal.UserID
	if h.auth != nil {
		if me, err := h.auth.Me(c.Request.Context(), principal.UserID); err == nil {
			// The username, not the id, so the cluster's audit log names a
			// person.
			username = me.Username
		}
	}
	kc, err := h.clusters.MintKubeconfig(c.Request.Context(), c.Param("id"), username, principal.Role)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, kubeconfigResponse{
		Kubeconfig: string(kc.Kubeconfig.Bytes()), Group: kc.Group, ExpiresAt: kc.ExpiresAt,
	})
}
