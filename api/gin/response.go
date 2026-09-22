package ginapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/helpers/api"
)

// Object sends a single resource. The resource carries its own "object" field.
func Object(c *gin.Context, obj any) { c.JSON(http.StatusOK, obj) }

// Created sends 201 with the created resource.
func Created(c *gin.Context, obj any) { c.JSON(http.StatusCreated, obj) }

// NoContent sends 204.
func NoContent(c *gin.Context) { c.Status(http.StatusNoContent) }

// Deleted sends a deletion confirmation.
func Deleted(c *gin.Context, objectType, id string) {
	c.JSON(http.StatusOK, api.NewDeleted(objectType, id))
}

// Success sends 200 with a bare human-readable message.
func Success(c *gin.Context, message string) {
	c.JSON(http.StatusOK, api.NewMessage(message))
}

// ListResponse sends a list body with has_more computed.
func ListResponse[T any](c *gin.Context, data []T, total int64, limit, offset int) {
	c.JSON(http.StatusOK, api.NewList(data, total, limit, offset))
}
