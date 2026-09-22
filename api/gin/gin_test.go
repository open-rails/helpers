package ginapi_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/helpers/api"
	ginapi "github.com/open-rails/helpers/api/gin"
)

// call runs h on a real gin engine behind a real listener and returns what a
// client reads off the socket.
func call(t *testing.T, target string, h gin.HandlerFunc) (int, string) {
	t.Helper()
	r := gin.New()
	r.GET("/x", h)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + target)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp.StatusCode, string(body)
}

// Each case is a writer, the status it must send, and the exact bytes. The
// bytes are the canonical api envelope: no top-level discriminator,
// application/json with no charset parameter, one trailing newline — the same
// bytes the root net/http writer emits, because it IS the root writer.
func TestErrorWriters(t *testing.T) {
	cases := []struct {
		name   string
		h      gin.HandlerFunc
		status int
		body   string
	}{
		{"BadRequest", func(c *gin.Context) { ginapi.BadRequest(c, "bad") }, 400,
			`{"error":{"type":"invalid_request_error","message":"bad"}}`},
		{"BadRequestWithCode", func(c *gin.Context) { ginapi.BadRequestWithCode(c, api.CodeInvalidFormat, "bad") }, 400,
			`{"error":{"type":"invalid_request_error","code":"invalid_format","message":"bad"}}`},
		{"BadRequestParam", func(c *gin.Context) { ginapi.BadRequestParam(c, "limit", "must be positive") }, 400,
			`{"error":{"type":"invalid_request_error","message":"must be positive","param":"limit"}}`},
		{"Unauthorized", ginapi.Unauthorized, 401,
			`{"error":{"type":"authentication_error","message":"unauthorized"}}`},
		{"UnauthorizedWithMessage", func(c *gin.Context) { ginapi.UnauthorizedWithMessage(c, "token expired") }, 401,
			`{"error":{"type":"authentication_error","message":"token expired"}}`},
		{"Forbidden", ginapi.Forbidden, 403,
			`{"error":{"type":"authorization_error","message":"forbidden"}}`},
		{"ForbiddenWithMessage", func(c *gin.Context) { ginapi.ForbiddenWithMessage(c, "not yours") }, 403,
			`{"error":{"type":"authorization_error","message":"not yours"}}`},
		{"NotFound", func(c *gin.Context) { ginapi.NotFound(c, "gallery") }, 404,
			`{"error":{"type":"invalid_request_error","message":"gallery not found"}}`},
		{"NotFoundWithMessage", func(c *gin.Context) { ginapi.NotFoundWithMessage(c, "gone") }, 404,
			`{"error":{"type":"invalid_request_error","message":"gone"}}`},
		{"Conflict", func(c *gin.Context) { ginapi.Conflict(c, "taken") }, 409,
			`{"error":{"type":"invalid_request_error","message":"taken"}}`},
		{"UnsupportedMediaType", func(c *gin.Context) { ginapi.UnsupportedMediaType(c, "send json") }, 415,
			`{"error":{"type":"invalid_request_error","message":"send json"}}`},
		{"UnprocessableEntity", func(c *gin.Context) { ginapi.UnprocessableEntity(c, "empty chapter") }, 422,
			`{"error":{"type":"invalid_request_error","message":"empty chapter"}}`},
		{"ModerationRejected", func(c *gin.Context) { ginapi.ModerationRejected(c, "policy: minors") }, 422,
			`{"error":{"type":"invalid_request_error","code":"moderation_rejected","message":"policy: minors"}}`},
		{"TooManyRequests", func(c *gin.Context) { ginapi.TooManyRequests(c, "slow down") }, 429,
			`{"error":{"type":"rate_limit_error","message":"slow down"}}`},
		{"InternalError", func(c *gin.Context) { ginapi.InternalError(c, `pq: relation "x" does not exist`) }, 500,
			`{"error":{"type":"api_error","code":"internal_error","message":"internal error"}}`},
		{"NotImplemented", func(c *gin.Context) { ginapi.NotImplemented(c, "not built") }, 501,
			`{"error":{"type":"api_error","code":"not_implemented","message":"not built"}}`},
		{"NotConfigured", func(c *gin.Context) { ginapi.NotConfigured(c, "no MediaStore configured") }, 501,
			`{"error":{"type":"api_error","code":"not_configured","message":"no MediaStore configured"}}`},
		{"BadGateway", func(c *gin.Context) { ginapi.BadGateway(c, "upstream down") }, 502,
			`{"error":{"type":"api_error","message":"upstream down"}}`},
		{"ServiceUnavailable", func(c *gin.Context) { ginapi.ServiceUnavailable(c, "maintenance") }, 503,
			`{"error":{"type":"api_error","message":"maintenance"}}`},
		{"Fail renders an api.Error", func(c *gin.Context) {
			ginapi.Fail(c, api.E(http.StatusNotFound, api.CodeResourceNotFound, "no such artist"))
		}, 404,
			`{"error":{"type":"invalid_request_error","code":"resource_not_found","message":"no such artist"}}`},
		{"Fail scrubs an unexpected error", func(c *gin.Context) {
			ginapi.Fail(c, io.ErrUnexpectedEOF)
		}, 500,
			`{"error":{"type":"api_error","code":"internal_error","message":"internal error"}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := call(t, "/x", tc.h)
			if status != tc.status {
				t.Errorf("status = %d, want %d", status, tc.status)
			}
			if body != tc.body+"\n" {
				t.Errorf("wire body mismatch\n got: %s\nwant: %s", body, tc.body)
			}
		})
	}
}

// The adapter must not become a second implementation of the envelope. Every
// writer is driven through both surfaces and the bytes are compared.
func TestGinWritersAreByteIdenticalToNetHTTP(t *testing.T) {
	cases := []struct {
		name string
		gin  gin.HandlerFunc
		err  error
	}{
		{"400", func(c *gin.Context) { ginapi.BadRequestParam(c, "limit", "must be positive") },
			api.E(http.StatusBadRequest, "", "must be positive").WithParam("limit")},
		{"401", ginapi.Unauthorized, api.E(http.StatusUnauthorized, "", "unauthorized")},
		{"403", ginapi.Forbidden, api.E(http.StatusForbidden, "", "forbidden")},
		{"404", func(c *gin.Context) { ginapi.NotFound(c, "gallery") }, api.E(http.StatusNotFound, "", "gallery not found")},
		{"409", func(c *gin.Context) { ginapi.Conflict(c, "taken") }, api.E(http.StatusConflict, "", "taken")},
		{"422", func(c *gin.Context) { ginapi.ModerationRejected(c, "policy") },
			api.E(http.StatusUnprocessableEntity, api.CodeModerationRejected, "policy")},
		{"429", func(c *gin.Context) { ginapi.TooManyRequests(c, "slow down") }, api.E(http.StatusTooManyRequests, "", "slow down")},
		{"500", func(c *gin.Context) { ginapi.InternalError(c, "boom") }, api.E(http.StatusInternalServerError, "", "boom")},
		{"501", func(c *gin.Context) { ginapi.NotImplemented(c, "not built") },
			api.E(http.StatusNotImplemented, api.CodeNotImplemented, "not built")},
		{"503", func(c *gin.Context) { ginapi.ServiceUnavailable(c, "maintenance") }, api.E(http.StatusServiceUnavailable, "", "maintenance")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ginStatus, ginBody, ginType := callFull(t, tc.gin)
			plainStatus, plainBody, plainType := plain(t, func(w http.ResponseWriter, _ *http.Request) { api.WriteError(w, tc.err) })
			if ginStatus != plainStatus || ginBody != plainBody || ginType != plainType {
				t.Errorf("gin and net/http disagree\n gin: %d %s %q\nhttp: %d %s %q",
					ginStatus, ginType, ginBody, plainStatus, plainType, plainBody)
			}
		})
	}
}

func callFull(t *testing.T, h gin.HandlerFunc) (int, string, string) {
	t.Helper()
	r := gin.New()
	r.GET("/x", h)
	return request(t, r)
}

func plain(t *testing.T, h http.HandlerFunc) (int, string, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/x", h)
	return request(t, mux)
}

func request(t *testing.T, h http.Handler) (int, string, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp.StatusCode, string(body), resp.Header.Get("Content-Type")
}

func TestSuccessWriters(t *testing.T) {
	type artist struct {
		Object string `json:"object"`
		ID     string `json:"id"`
	}
	cases := []struct {
		name   string
		h      gin.HandlerFunc
		status int
		body   string
	}{
		{"Object", func(c *gin.Context) { ginapi.Object(c, artist{"artist", "a_1"}) }, 200, `{"object":"artist","id":"a_1"}`},
		{"Created", func(c *gin.Context) { ginapi.Created(c, artist{"artist", "a_1"}) }, 201, `{"object":"artist","id":"a_1"}`},
		{"NoContent", ginapi.NoContent, 204, ``},
		{"Deleted", func(c *gin.Context) { ginapi.Deleted(c, "artist", "a_1") }, 200, `{"object":"artist","id":"a_1","deleted":true}`},
		{"Success", func(c *gin.Context) { ginapi.Success(c, "saved") }, 200, `{"object":"message","message":"saved"}`},
		{"ListResponse", func(c *gin.Context) { ginapi.ListResponse(c, []string{"a"}, 3, 1, 0) }, 200,
			`{"object":"list","data":["a"],"total":3,"limit":1,"offset":0,"has_more":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := call(t, "/x", tc.h)
			if status != tc.status {
				t.Errorf("status = %d, want %d", status, tc.status)
			}
			if body != tc.body {
				t.Errorf("got %s want %s", body, tc.body)
			}
		})
	}
}

func TestBind(t *testing.T) {
	var got api.ListParams
	call(t, "/x?limit=5&offset=10&sort=name", func(c *gin.Context) { got = ginapi.Bind(c); c.Status(200) })
	if got != (api.ListParams{Limit: 5, Offset: 10, Sort: "name"}) {
		t.Errorf("got %+v", got)
	}
}

func TestBindFallsBackToSortBy(t *testing.T) {
	var got api.ListParams
	call(t, "/x?sort_by=created_at", func(c *gin.Context) { got = ginapi.Bind(c); c.Status(200) })
	if got.Sort != "created_at" {
		t.Errorf("got %q, want created_at", got.Sort)
	}
}

func TestBindDefaultNormalizes(t *testing.T) {
	var got api.ListParams
	call(t, "/x?limit=9999&offset=-4", func(c *gin.Context) { got = ginapi.BindDefault(c); c.Status(200) })
	if got.Limit != api.MaxLimit || got.Offset != 0 {
		t.Errorf("got %+v, want limit=%d offset=0", got, api.MaxLimit)
	}

	call(t, "/x", func(c *gin.Context) { got = ginapi.BindDefault(c); c.Status(200) })
	if got.Limit != api.DefaultLimit {
		t.Errorf("got limit=%d, want %d", got.Limit, api.DefaultLimit)
	}
}

func TestBindWithDefaults(t *testing.T) {
	var got api.ListParams
	call(t, "/x?limit=500", func(c *gin.Context) { got = ginapi.BindWithDefaults(c, 10, 50); c.Status(200) })
	if got.Limit != 50 {
		t.Errorf("got limit=%d, want the caller's cap of 50", got.Limit)
	}
}
