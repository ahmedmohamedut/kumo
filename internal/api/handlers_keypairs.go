package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api/middleware"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
)

// createKeyPairRequest is the body of POST /key-pairs.
type createKeyPairRequest struct {
	Name string            `json:"name"`
	Type keypair.Type      `json:"type"`
	Tags map[string]string `json:"tags"`
}

// importKeyPairRequest is the body of POST /key-pairs/import.
type importKeyPairRequest struct {
	Name      string            `json:"name"`
	PublicKey string            `json:"public_key"`
	Tags      map[string]string `json:"tags"`
}

// updateKeyPairRequest is the body of PATCH /key-pairs/{id}: tags only
// (docs/design/01-api.md §Key pairs).
type updateKeyPairRequest struct {
	Tags map[string]string `json:"tags"`
}

// listKeyPairs answers GET /key-pairs with one page of key pairs.
func (h *Handler) listKeyPairs(c *gin.Context) {
	input := keypair.ListInput{
		Query:     c.Query("q"),
		Sort:      keypair.SortOrder(c.Query("sort")),
		PageToken: c.Query("page_token"),
	}

	if raw := c.Query("page_size"); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < keypair.MinPageSize || size > keypair.MaxPageSize {
			WriteError(c, h.logger, Invalid("page_size is not acceptable",
				FieldError{Field: "page_size", Reason: fmt.Sprintf(
					"must be between %d and %d", keypair.MinPageSize, keypair.MaxPageSize)}))
			return
		}
		input.PageSize = size
	}

	page, err := h.keyPairs.List(c.Request.Context(), input)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, page)
}

// createKeyPair answers POST /key-pairs by generating a key pair. The
// private key rides along in this one response; the body is bound but never
// logged beyond the field validation errors.
func (h *Handler) createKeyPair(c *gin.Context) {
	var req createKeyPairRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with a name"}))
		return
	}

	created, err := h.keyPairs.Create(c.Request.Context(), keypair.CreateInput{
		Name:      req.Name,
		Type:      req.Type,
		Tags:      req.Tags,
		CreatedBy: middleware.PrincipalFrom(c).UserID,
	})
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusCreated, created)
}

// importKeyPair answers POST /key-pairs/import with an existing public key.
func (h *Handler) importKeyPair(c *gin.Context) {
	var req importKeyPairRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with a name and a public_key"}))
		return
	}

	pair, err := h.keyPairs.Import(c.Request.Context(), keypair.ImportInput{
		Name:      req.Name,
		PublicKey: req.PublicKey,
		Tags:      req.Tags,
		CreatedBy: middleware.PrincipalFrom(c).UserID,
	})
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusCreated, pair)
}

// getKeyPair answers GET /key-pairs/{id}.
func (h *Handler) getKeyPair(c *gin.Context) {
	detail, err := h.keyPairs.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, detail)
}

// updateKeyPair answers PATCH /key-pairs/{id}, which replaces the tag set.
func (h *Handler) updateKeyPair(c *gin.Context) {
	var req updateKeyPairRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with tags"}))
		return
	}
	if req.Tags == nil {
		WriteError(c, h.logger, Invalid("the request updates nothing",
			FieldError{Field: "tags", Reason: "must be present; pass {} to remove all tags"}))
		return
	}

	pair, err := h.keyPairs.UpdateTags(c.Request.Context(), c.Param("id"), req.Tags)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, pair)
}

// deleteKeyPair answers DELETE /key-pairs/{id}. ?force=true overrides the
// in-use protection (docs/design/01-api.md).
func (h *Handler) deleteKeyPair(c *gin.Context) {
	force := c.Query("force") == "true"

	if err := h.keyPairs.Delete(c.Request.Context(), c.Param("id"), force); err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.Status(http.StatusNoContent)
}
