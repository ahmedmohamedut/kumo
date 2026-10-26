package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api/middleware"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/bucket"
)

// bucketsResponse is the body of GET /api/v1/s3/buckets. The list is not
// paged: it is read from the object store, which returns every bucket at once.
type bucketsResponse struct {
	Items []bucket.Bucket `json:"items"`
}

// createBucketRequest is the body of POST /s3/buckets.
type createBucketRequest struct {
	Name       string            `json:"name"`
	Versioning bool              `json:"versioning"`
	Tags       map[string]string `json:"tags"`
}

// updateBucketRequest is the body of PATCH /s3/buckets/{name}. An absent
// field is left alone; "tags": {} drops every tag.
type updateBucketRequest struct {
	Versioning *bool              `json:"versioning"`
	Tags       *map[string]string `json:"tags"`
}

// objectStorageOff answers every /s3 route on a node without object storage,
// so a client learns the module is off rather than that the path is wrong.
func (h *Handler) objectStorageOff(c *gin.Context) {
	WriteError(c, h.logger, fmt.Errorf("%w: object storage is not configured on this node; "+
		"set KUMO_OBJECTSTORE (docs/design/06-object-storage.md)", service.ErrUnavailable))
	c.Abort()
}

// listBuckets answers GET /s3/buckets.
func (h *Handler) listBuckets(c *gin.Context) {
	buckets, err := h.buckets.List(c.Request.Context())
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.JSON(http.StatusOK, bucketsResponse{Items: buckets})
}

// getBucket answers GET /s3/buckets/{name}.
func (h *Handler) getBucket(c *gin.Context) {
	detail, err := h.buckets.Get(c.Request.Context(), c.Param("name"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

// createBucket answers POST /s3/buckets. A create is a few fast requests to
// the object store, so it answers 201 with the bucket rather than a task.
func (h *Handler) createBucket(c *gin.Context) {
	var req createBucketRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with a name"}))
		return
	}

	created, err := h.buckets.Create(c.Request.Context(), bucket.CreateInput{
		Name:       req.Name,
		Versioning: req.Versioning,
		Tags:       req.Tags,
		CreatedBy:  middleware.PrincipalFrom(c).UserID,
	})
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.Header("Location", "/api/v1/s3/buckets/"+created.Name)
	c.JSON(http.StatusCreated, created)
}

// updateBucket answers PATCH /s3/buckets/{name}: versioning and tags. The
// name is immutable, as in S3.
func (h *Handler) updateBucket(c *gin.Context) {
	var req updateBucketRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with versioning or tags"}))
		return
	}
	if req.Versioning == nil && req.Tags == nil {
		WriteError(c, h.logger, Invalid("the request updates nothing",
			FieldError{Field: "body", Reason: "must set versioning or tags"}))
		return
	}

	in := bucket.UpdateInput{Versioning: req.Versioning}
	if req.Tags != nil {
		in.SetTags = true
		in.Tags = *req.Tags
	}
	updated, err := h.buckets.Update(c.Request.Context(), c.Param("name"), in)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.JSON(http.StatusOK, updated)
}

// deleteBucket answers DELETE /s3/buckets/{name}: 204 for an empty bucket,
// 409 for one that holds anything, and with ?force=true 202 and a task that
// empties it first.
func (h *Handler) deleteBucket(c *gin.Context) {
	force, ok := forceFlag(c, h)
	if !ok {
		return
	}
	ctx, name := c.Request.Context(), c.Param("name")

	if !force {
		if err := h.buckets.Delete(ctx, name); err != nil {
			WriteError(c, h.logger, err)
			return
		}
		c.Status(http.StatusNoContent)
		return
	}

	deleted, err := h.buckets.ForceDelete(ctx, name)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.Header("Location", "/api/v1/tasks/"+deleted.TaskID)
	c.JSON(http.StatusAccepted, deleted)
}

// presignUploadRequest is the body of POST /s3/buckets/{name}/uploads.
type presignUploadRequest struct {
	Key         string `json:"key"`
	ContentType string `json:"content_type"`
}

// presignDownloadRequest is the body of POST /s3/buckets/{name}/downloads.
type presignDownloadRequest struct {
	Key string `json:"key"`
}

// createFolderRequest is the body of POST /s3/buckets/{name}/folders.
type createFolderRequest struct {
	Prefix string `json:"prefix"`
}

// folderResponse is the answer to a folder create.
type folderResponse struct {
	Prefix string `json:"prefix"`
}

// deleteObjectsRequest is the body of POST /s3/buckets/{name}/objects/delete.
type deleteObjectsRequest struct {
	Keys []string `json:"keys"`
}

// listObjects answers GET /s3/buckets/{name}/objects?prefix=&cursor=&limit=.
func (h *Handler) listObjects(c *gin.Context) {
	limit := 0
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			WriteError(c, h.logger, Invalid("limit is not acceptable",
				FieldError{Field: "limit", Reason: "must be a number"}))
			return
		}
		limit = parsed
	}
	listing, err := h.buckets.ListObjects(c.Request.Context(), c.Param("name"), bucket.ListObjectsInput{
		Prefix: c.Query("prefix"),
		Cursor: c.Query("cursor"),
		Limit:  limit,
	})
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.JSON(http.StatusOK, listing)
}

// presignUpload answers POST /s3/buckets/{name}/uploads with a URL the
// browser PUTs the file to directly.
func (h *Handler) presignUpload(c *gin.Context) {
	var req presignUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with a key"}))
		return
	}
	signed, err := h.buckets.PresignUpload(c.Request.Context(), c.Param("name"), req.Key, req.ContentType)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.JSON(http.StatusOK, signed)
}

// presignDownload answers POST /s3/buckets/{name}/downloads. It is a POST,
// not a GET, so a URL that grants access is never cached or logged as the
// address of a page.
func (h *Handler) presignDownload(c *gin.Context) {
	var req presignDownloadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with a key"}))
		return
	}
	signed, err := h.buckets.PresignDownload(c.Request.Context(), c.Param("name"), req.Key)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.JSON(http.StatusOK, signed)
}

// createFolder answers POST /s3/buckets/{name}/folders.
func (h *Handler) createFolder(c *gin.Context) {
	var req createFolderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with a prefix"}))
		return
	}
	folder, err := h.buckets.CreateFolder(c.Request.Context(), c.Param("name"), req.Prefix)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.JSON(http.StatusCreated, folderResponse{Prefix: folder})
}

// deleteObjects answers POST /s3/buckets/{name}/objects/delete, the batch
// delete of S3. A POST carries the key list, which a DELETE body cannot be
// relied on to.
func (h *Handler) deleteObjects(c *gin.Context) {
	var req deleteObjectsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with a list of keys"}))
		return
	}
	if err := h.buckets.DeleteObjects(c.Request.Context(), c.Param("name"), req.Keys); err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.Status(http.StatusNoContent)
}
