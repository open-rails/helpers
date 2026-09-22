package api

// List is the offset/limit list body: {"object":"list","data":[...],...}.
type List[T any] struct {
	Object  string `json:"object"` // always "list"
	Data    []T    `json:"data"`
	Total   int64  `json:"total"`
	Limit   int    `json:"limit"`
	Offset  int    `json:"offset"`
	HasMore bool   `json:"has_more"`
}

// NewList builds a list body, computing has_more and never emitting a null data.
func NewList[T any](data []T, total int64, limit, offset int) List[T] {
	if data == nil {
		data = []T{}
	}
	return List[T]{
		Object:  "list",
		Data:    data,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
		HasMore: HasMoreFromLen(offset, len(data), total),
	}
}

// DeletedObject confirms a deletion: {"object":"gallery","id":"…","deleted":true}.
type DeletedObject struct {
	Object  string `json:"object"`
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
}

// NewDeleted builds a deletion confirmation.
func NewDeleted(objectType, id string) DeletedObject {
	return DeletedObject{Object: objectType, ID: id, Deleted: true}
}

// Message is a bare human-readable success body.
type Message struct {
	Object  string `json:"object"` // always "message"
	Message string `json:"message"`
}

// NewMessage builds a message body.
func NewMessage(message string) Message { return Message{Object: "message", Message: message} }
