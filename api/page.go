package api

// Offset pagination defaults.
const (
	DefaultLimit = 20
	MaxLimit     = 100
)

// ListParams is the offset/limit/sort input a list endpoint reads from the
// request. Framework bindings fill it; see adapters/gin.
type ListParams struct {
	Limit  int
	Offset int
	Sort   string
}

// Normalize applies the caller's default and cap and floors the offset.
func (p *ListParams) Normalize(defaultLimit, maxLimit int) {
	if p.Limit <= 0 {
		p.Limit = defaultLimit
	}
	if maxLimit > 0 && p.Limit > maxLimit {
		p.Limit = maxLimit
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
}

// HasMore reports whether items follow a full page of limit items.
func HasMore(offset, limit int, total int64) bool {
	return int64(offset+limit) < total
}

// HasMoreFromLen reports whether items follow, using the rows actually read —
// correct when the last page is short.
func HasMoreFromLen(offset, resultLen int, total int64) bool {
	return int64(offset+resultLen) < total
}
