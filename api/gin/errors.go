// Package ginapi writes api envelopes, lists and objects through a
// gin.Context, binds list parameters from the query string, and carries the
// locale middleware. The envelope vocabulary itself lives in api, which
// depends on nothing.
//
// Every writer here is byte-for-byte identical to the root net/http writer for
// the same error — gin_test.go asserts it off a real socket.
package ginapi

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/helpers/api"
)

// Fail writes err as the canonical envelope, deriving status and shape from it.
// It is api.WriteError against c.Writer — the same function, not a parallel
// implementation, which is how byte-for-byte equivalence is guaranteed rather
// than tested for.
func Fail(c *gin.Context, err error) { api.WriteError(c.Writer, err) }

// Send writes an explicit envelope. Code may be empty: an absent code means
// "no machine reason beyond the status", and the writers never invent one.
// param is a plain string — the wire's pointer is api's problem.
func Send(c *gin.Context, status int, t api.Type, code api.Code, message, param string) {
	e := (&api.Error{Status: status, Type: t, Code: code, Message: message}).WithParam(param)
	Fail(c, e)
}

func send(c *gin.Context, status int, code api.Code, message, param string) {
	Send(c, status, api.TypeForStatus(status), code, message, param)
}

// BadRequest sends 400 invalid_request_error.
func BadRequest(c *gin.Context, message string) {
	send(c, http.StatusBadRequest, "", message, "")
}

// BadRequestWithCode sends 400 with a machine-readable code.
func BadRequestWithCode(c *gin.Context, code api.Code, message string) {
	send(c, http.StatusBadRequest, code, message, "")
}

// BadRequestParam sends 400 naming the offending request field.
func BadRequestParam(c *gin.Context, param, message string) {
	send(c, http.StatusBadRequest, "", message, param)
}

// Unauthorized sends 401 authentication_error.
func Unauthorized(c *gin.Context) { UnauthorizedWithMessage(c, "unauthorized") }

// UnauthorizedWithMessage sends 401 with a custom message.
func UnauthorizedWithMessage(c *gin.Context, message string) {
	send(c, http.StatusUnauthorized, "", message, "")
}

// Forbidden sends 403 authorization_error.
func Forbidden(c *gin.Context) { ForbiddenWithMessage(c, "forbidden") }

// ForbiddenWithMessage sends 403 with a custom message.
func ForbiddenWithMessage(c *gin.Context, message string) {
	send(c, http.StatusForbidden, "", message, "")
}

// NotFound sends 404 for a named entity. The transport type stays
// invalid_request_error, as authkit and openrails deploy it; resource_not_found
// is the code a client branches on.
func NotFound(c *gin.Context, entity string) {
	NotFoundWithMessage(c, fmt.Sprintf("%s not found", entity))
}

// NotFoundWithMessage sends 404 with a custom message.
func NotFoundWithMessage(c *gin.Context, message string) {
	send(c, http.StatusNotFound, "", message, "")
}

// Conflict sends 409. The state conflict is in the code, not a transport type.
func Conflict(c *gin.Context, message string) {
	send(c, http.StatusConflict, "", message, "")
}

// ModerationRejected sends 422 invalid_request_error with the stable code
// moderation_rejected: transport classification is unchanged, and the code is
// what tells a SPA to show the author a reason instead of retrying.
func ModerationRejected(c *gin.Context, reason string) {
	send(c, http.StatusUnprocessableEntity, api.CodeModerationRejected, reason, "")
}

// UnprocessableEntity sends 422 invalid_request_error: well-formed syntax the
// server cannot act on. For a moderation refusal use ModerationRejected.
func UnprocessableEntity(c *gin.Context, message string) {
	send(c, http.StatusUnprocessableEntity, "", message, "")
}

// UnsupportedMediaType sends 415 invalid_request_error.
func UnsupportedMediaType(c *gin.Context, message string) {
	send(c, http.StatusUnsupportedMediaType, "", message, "")
}

// TooManyRequests sends 429 rate_limit_error.
func TooManyRequests(c *gin.Context, message string) {
	send(c, http.StatusTooManyRequests, "", message, "")
}

// InternalError sends 500 api_error. The message is scrubbed: 500 is the one
// status that means "unexpected", so its text may carry an internal cause.
func InternalError(c *gin.Context, message string) {
	send(c, http.StatusInternalServerError, "", message, "")
}

// BadGateway sends 502 api_error for an upstream failure.
func BadGateway(c *gin.Context, message string) {
	send(c, http.StatusBadGateway, "", message, "")
}

// ServiceUnavailable sends 503 api_error.
func ServiceUnavailable(c *gin.Context, message string) {
	send(c, http.StatusServiceUnavailable, "", message, "")
}

// NotImplemented sends 501 api_error with the stable code not_implemented:
// this build lacks the capability. No new transport type — the code is the
// signal to hide the feature rather than retry.
func NotImplemented(c *gin.Context, message string) {
	send(c, http.StatusNotImplemented, api.CodeNotImplemented, message, "")
}

// NotConfigured sends 501 api_error for an optional capability the operator
// never wired — a deployment gap, distinguishable from a crash by its code.
func NotConfigured(c *gin.Context, message string) {
	send(c, http.StatusNotImplemented, api.CodeNotConfigured, message, "")
}
