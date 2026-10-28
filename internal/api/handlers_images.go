package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api/middleware"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
)

// importImageRequest is the body of POST /images/import.
type importImageRequest struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	SourceURL    string            `json:"source_url"`
	Checksum     *checksumRequest  `json:"checksum"`
	OSFamily     string            `json:"os_family"`
	OSVersion    string            `json:"os_version"`
	Architecture string            `json:"architecture"`
	DefaultUser  string            `json:"default_user"`
	MinDiskGB    int               `json:"min_disk_gb"`
	Tags         map[string]string `json:"tags"`
	// Kind, Engine and EngineVersion import a golden image for the databases
	// module. Omitting Kind imports an ordinary image, so every client written
	// before M7.2 keeps working (docs/design/07-databases.md section 3).
	Kind          string `json:"kind"`
	Engine        string `json:"engine"`
	EngineVersion string `json:"engine_version"`
}

// checksumRequest is the checksum block of an import body.
type checksumRequest struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

// updateImageRequest is the body of PATCH /images/{id}. A nil field is left
// alone, so the three pointers are what distinguishes "unchanged" from "cleared".
type updateImageRequest struct {
	Name        *string           `json:"name"`
	Description *string           `json:"description"`
	Tags        map[string]string `json:"tags"`
}

// listImages answers GET /images.
func (h *Handler) listImages(c *gin.Context) {
	input := image.ListInput{
		Query:     c.Query("q"),
		State:     image.State(c.Query("state")),
		Sort:      image.SortOrder(c.Query("sort")),
		PageToken: c.Query("page_token"),
	}

	if raw := c.Query("page_size"); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < image.MinPageSize || size > image.MaxPageSize {
			WriteError(c, h.logger, Invalid("page_size is not acceptable",
				FieldError{Field: "page_size", Reason: fmt.Sprintf(
					"must be between %d and %d", image.MinPageSize, image.MaxPageSize)}))
			return
		}
		input.PageSize = size
	}

	page, err := h.images.List(c.Request.Context(), input)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, page)
}

// importImage answers POST /images/import. The download is long, so the response
// is 202 with the image in pending and a Location pointing at the task
// (docs/design/01-api.md §Images).
func (h *Handler) importImage(c *gin.Context) {
	var req importImageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with a name and a source_url"}))
		return
	}

	in := image.ImportInput{
		Name:          req.Name,
		Description:   req.Description,
		SourceURL:     req.SourceURL,
		OSFamily:      req.OSFamily,
		OSVersion:     req.OSVersion,
		Architecture:  image.Architecture(req.Architecture),
		DefaultUser:   req.DefaultUser,
		MinDiskGB:     req.MinDiskGB,
		Tags:          req.Tags,
		CreatedBy:     middleware.PrincipalFrom(c).UserID,
		Kind:          image.Kind(req.Kind),
		Engine:        image.Engine(req.Engine),
		EngineVersion: req.EngineVersion,
	}
	if req.Checksum != nil {
		in.Checksum = image.Checksum{
			Algorithm: image.ChecksumAlgorithm(req.Checksum.Algorithm),
			Value:     req.Checksum.Value,
		}
	}

	imported, err := h.images.Import(c.Request.Context(), in)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.Header("Location", "/api/v1/tasks/"+imported.TaskID)
	c.JSON(http.StatusAccepted, imported)
}

// getImage answers GET /images/{id}.
func (h *Handler) getImage(c *gin.Context) {
	img, err := h.images.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, img)
}

// updateImage answers PATCH /images/{id}. Only the name, the description and the
// tags are writable; the import columns are not reachable.
func (h *Handler) updateImage(c *gin.Context) {
	var req updateImageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with name, description or tags"}))
		return
	}
	if req.Name == nil && req.Description == nil && req.Tags == nil {
		WriteError(c, h.logger, Invalid("the request updates nothing",
			FieldError{Field: "body", Reason: "must set name, description or tags"}))
		return
	}

	updated, err := h.images.Update(c.Request.Context(), c.Param("id"), image.UpdateInput{
		Name:        req.Name,
		Description: req.Description,
		Tags:        req.Tags,
	})
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, updated)
}

// deleteImage answers DELETE /images/{id}. Removing the volume is long, so the
// response is 202 with the image in deleting and a Location pointing at the task.
// An image an instance still boots from is a 409.
func (h *Handler) deleteImage(c *gin.Context) {
	deleted, err := h.images.Delete(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.Header("Location", "/api/v1/tasks/"+deleted.TaskID)
	c.JSON(http.StatusAccepted, deleted)
}

// retryImageImport answers POST /images/{id}/retry, which queues a new import
// for a failed image using the source URL it already carries.
func (h *Handler) retryImageImport(c *gin.Context) {
	retried, err := h.images.ResetFailed(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.Header("Location", "/api/v1/tasks/"+retried.TaskID)
	c.JSON(http.StatusAccepted, retried)
}

// listImageCatalog answers GET /images/catalog with the curated cloud images the
// console offers before the user pastes a URL.
func (h *Handler) listImageCatalog(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"items": h.images.Catalog(c.Request.Context())})
}
