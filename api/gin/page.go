package ginapi

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/helpers/api"
)

// Bind reads limit, offset and sort (or sort_by) from the query string,
// unnormalized.
func Bind(c *gin.Context) api.ListParams {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	sort := c.Query("sort")
	if sort == "" {
		sort = c.Query("sort_by")
	}
	return api.ListParams{Limit: limit, Offset: offset, Sort: sort}
}

// BindWithDefaults reads and normalizes list parameters against the caller's
// default and cap.
func BindWithDefaults(c *gin.Context, defaultLimit, maxLimit int) api.ListParams {
	p := Bind(c)
	p.Normalize(defaultLimit, maxLimit)
	return p
}

// BindDefault reads and normalizes against api's defaults (20, cap 100).
func BindDefault(c *gin.Context) api.ListParams {
	return BindWithDefaults(c, api.DefaultLimit, api.MaxLimit)
}
