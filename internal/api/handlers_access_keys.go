package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api/middleware"
	"github.com/ahmedmohamedut/kumo/internal/objectstore"
	"github.com/ahmedmohamedut/kumo/internal/service/accesskey"
)

// accessKeysResponse is the body of GET /s3/access-keys.
type accessKeysResponse struct {
	Items []accesskey.AccessKey `json:"items"`
}

// createAccessKeyRequest is the body of POST /s3/access-keys.
type createAccessKeyRequest struct {
	Name    string   `json:"name"`
	Level   string   `json:"level"`
	Buckets []string `json:"buckets"`
}

// updateAccessKeyRequest is the body of PATCH /s3/access-keys/{id}.
type updateAccessKeyRequest struct {
	Name    *string   `json:"name"`
	Level   *string   `json:"level"`
	Buckets *[]string `json:"buckets"`
}

// listAccessKeys answers GET /s3/access-keys. No answer carries a secret.
func (h *Handler) listAccessKeys(c *gin.Context) {
	keys, err := h.accessKeys.List(c.Request.Context())
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.JSON(http.StatusOK, accessKeysResponse{Items: keys})
}

// getAccessKey answers GET /s3/access-keys/{id}.
func (h *Handler) getAccessKey(c *gin.Context) {
	key, err := h.accessKeys.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.JSON(http.StatusOK, key)
}

// createAccessKey answers POST /s3/access-keys with the key and its secret.
// This answer is the only place the secret ever appears, so it must not be
// cached anywhere on the way, the same rule as a generated private key.
func (h *Handler) createAccessKey(c *gin.Context) {
	var req createAccessKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with a name, a level and buckets"}))
		return
	}
	created, err := h.accessKeys.Create(c.Request.Context(), accesskey.CreateInput{
		Name:      req.Name,
		Level:     objectstore.AccessLevel(req.Level),
		Buckets:   req.Buckets,
		CreatedBy: middleware.PrincipalFrom(c).UserID,
	})
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Location", "/api/v1/s3/access-keys/"+created.AccessKey.AccessKey)
	c.JSON(http.StatusCreated, created)
}

// updateAccessKey answers PATCH /s3/access-keys/{id}: name, level, buckets.
// The secret cannot be changed; a leaked key is deleted and reissued.
func (h *Handler) updateAccessKey(c *gin.Context) {
	var req updateAccessKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with name, level or buckets"}))
		return
	}
	if req.Name == nil && req.Level == nil && req.Buckets == nil {
		WriteError(c, h.logger, Invalid("the request updates nothing",
			FieldError{Field: "body", Reason: "must set name, level or buckets"}))
		return
	}
	in := accesskey.UpdateInput{Name: req.Name, Buckets: req.Buckets}
	if req.Level != nil {
		level := objectstore.AccessLevel(*req.Level)
		in.Level = &level
	}
	key, err := h.accessKeys.Update(c.Request.Context(), c.Param("id"), in)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.JSON(http.StatusOK, key)
}

// deleteAccessKey answers DELETE /s3/access-keys/{id}: the key stops working
// at once.
func (h *Handler) deleteAccessKey(c *gin.Context) {
	if err := h.accessKeys.Delete(c.Request.Context(), c.Param("id")); err != nil {
		WriteError(c, h.logger, err)
		return
	}
	c.Status(http.StatusNoContent)
}
