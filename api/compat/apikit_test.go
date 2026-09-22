package compat

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/helpers/api"
	ginapi "github.com/open-rails/helpers/api/gin"
)

// TestAPIKitCanonical records what THIS module emits, beside the three
// deployed writers, so the revision can be compared to what it replaces.
func TestAPIKitCanonical(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"400-bad-request", api.E(http.StatusBadRequest, "", "amount is required")},
		{"400-with-code", api.E(http.StatusBadRequest, api.CodeMissingParam, "amount is required")},
		{"400-with-param", api.E(http.StatusBadRequest, "", "amount is required").WithParam("amount")},
		{"401-unauthorized", api.E(http.StatusUnauthorized, "", "unauthorized")},
		{"402-card", api.E(http.StatusPaymentRequired, "insufficient_funds", "card declined")},
		{"403-forbidden", api.E(http.StatusForbidden, "", "forbidden")},
		{"404-not-found", api.E(http.StatusNotFound, "", "gallery not found")},
		{"409-conflict", api.E(http.StatusConflict, "", "already exists")},
		{"415-unsupported-media", api.E(http.StatusUnsupportedMediaType, "", "unsupported media type")},
		{"422-moderation-rejected", api.E(http.StatusUnprocessableEntity, api.CodeModerationRejected, "the artwork was refused")},
		{"429-rate-limit", api.E(http.StatusTooManyRequests, "", "slow down").WithMetadata(map[string]any{"retry_after": 30})},
		{"500-internal", api.E(http.StatusInternalServerError, "", `pq: constraint "galleries_slug_key" violated`)},
		{"501-not-implemented", api.E(http.StatusNotImplemented, api.CodeNotImplemented, "video transcoding is not configured")},
		{"502-bad-gateway", api.E(http.StatusBadGateway, "", "upstream failed")},
		{"503-unavailable", api.E(http.StatusServiceUnavailable, "", "try later")},
		{"with-request-id", api.E(http.StatusConflict, api.CodeResourceConflict, "idempotency key reused").WithRequestID("req_01HZX")},
		// The leak that must not reach the wire, recorded as bytes.
		{"metadata-scrubbed", api.E(http.StatusConflict, api.CodeResourceConflict, "conflict").
			WithMetadata(map[string]any{"constraint": `pq: duplicate key value violates unique constraint "users_email_key"`, "retry_after": 5})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := netHTTP(func(w http.ResponseWriter, _ *http.Request) { api.WriteError(w, tc.err) })
			golden(t, "apikit/"+tc.name, capture(t, h, nil))
		})
	}
}

// TestAPIKitCompatObject records the SAME errors in the GinAPI-compatible
// shape. Two directories, one difference: the top-level discriminator. Feed
// both to spa/probe.mjs and the consequence of the open decision is data.
func TestAPIKitCompatObject(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"400-bad-request", api.E(http.StatusBadRequest, "", "amount is required")},
		{"404-not-found", api.E(http.StatusNotFound, "", "gallery not found")},
		{"422-moderation-rejected", api.E(http.StatusUnprocessableEntity, api.CodeModerationRejected, "the artwork was refused")},
		{"500-internal", api.E(http.StatusInternalServerError, "", "boom")},
		{"501-not-implemented", api.E(http.StatusNotImplemented, api.CodeNotImplemented, "video transcoding is not configured")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := netHTTP(func(w http.ResponseWriter, _ *http.Request) {
				status, env := api.EnvelopeFor(tc.err)
				api.WriteJSON(w, status, api.WithObject(env))
			})
			golden(t, "apikit-compat-object/"+tc.name, capture(t, h, nil))
		})
	}
}

// TestAPIKitGinMatchesNetHTTP is the adapter's cross-module guarantee, asserted
// here as well because this module is the one that reads bytes off a socket for
// every writer in the fleet.
func TestAPIKitGinMatchesNetHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	statuses := []int{400, 401, 403, 404, 409, 415, 422, 429, 500, 501, 502, 503}
	for _, status := range statuses {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			err := api.E(status, "", "probe message")
			r := gin.New()
			r.GET("/probe", func(c *gin.Context) { ginapi.Fail(c, err) })
			viaGin := capture(t, r, nil)
			viaHTTP := capture(t, netHTTP(func(w http.ResponseWriter, _ *http.Request) { api.WriteError(w, err) }), nil)
			if viaGin != viaHTTP {
				t.Errorf("gin and net/http disagree at %d\n--- gin ---\n%s\n--- net/http ---\n%s", status, viaGin, viaHTTP)
			}
		})
	}
}
