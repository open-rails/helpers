package ginapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	ginapi "github.com/open-rails/helpers/api/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestLanguageFromQueryParam(t *testing.T) {
	router := gin.New()
	router.Use(ginapi.Language(ginapi.LanguageConfig{
		Supported: []string{"en", "ja", "ko"},
		Default:   "en",
	}))
	router.GET("/test", func(c *gin.Context) {
		lang := ginapi.GetLanguage(c)
		c.String(http.StatusOK, lang)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test?lang=ja", nil)
	router.ServeHTTP(w, req)

	if w.Body.String() != "ja" {
		t.Errorf("expected 'ja', got '%s'", w.Body.String())
	}
	if w.Header().Get("Content-Language") != "ja" {
		t.Errorf("expected Content-Language 'ja', got '%s'", w.Header().Get("Content-Language"))
	}
}

func TestLanguageFromPath(t *testing.T) {
	router := gin.New()
	router.Use(ginapi.Language(ginapi.LanguageConfig{
		Supported: []string{"en", "ja", "ko"},
		Default:   "en",
	}))
	router.GET("/ko/*path", func(c *gin.Context) {
		lang := ginapi.GetLanguage(c)
		c.String(http.StatusOK, lang)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/ko/galleries", nil)
	router.ServeHTTP(w, req)

	if w.Body.String() != "ko" {
		t.Errorf("expected 'ko', got '%s'", w.Body.String())
	}
}

func TestLanguageFromAcceptHeader(t *testing.T) {
	router := gin.New()
	router.Use(ginapi.Language(ginapi.LanguageConfig{
		Supported: []string{"en", "ja", "ko"},
		Default:   "en",
	}))
	router.GET("/test", func(c *gin.Context) {
		lang := ginapi.GetLanguage(c)
		c.String(http.StatusOK, lang)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("Accept-Language", "ko-KR,ko;q=0.9,en;q=0.8")
	router.ServeHTTP(w, req)

	if w.Body.String() != "ko" {
		t.Errorf("expected 'ko', got '%s'", w.Body.String())
	}
}

func TestLanguageDefault(t *testing.T) {
	router := gin.New()
	router.Use(ginapi.Language(ginapi.LanguageConfig{
		Supported: []string{"en", "ja"},
		Default:   "en",
	}))
	router.GET("/test", func(c *gin.Context) {
		lang := ginapi.GetLanguage(c)
		c.String(http.StatusOK, lang)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("Accept-Language", "fr,de")
	router.ServeHTTP(w, req)

	if w.Body.String() != "en" {
		t.Errorf("expected 'en', got '%s'", w.Body.String())
	}
}

// The language lives in the gin context only (the http-context storage was
// removed); GetLanguage is the whole read surface.
func TestGetLanguageFromGinContext(t *testing.T) {
	router := gin.New()
	router.Use(ginapi.Language(ginapi.LanguageConfig{Supported: []string{"en", "ja"}}))
	router.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, ginapi.GetLanguage(c)) })

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x?lang=ja", nil))
	if got := w.Body.String(); got != "ja" {
		t.Errorf("got %q want ja", got)
	}
	if got := w.Header().Get("Content-Language"); got != "ja" {
		t.Errorf("Content-Language = %q, want ja", got)
	}
}

func TestGetLanguageWithoutMiddleware(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if got := ginapi.GetLanguage(c); got != "en" {
		t.Errorf("got %q, want the en fallback", got)
	}
}

func TestLanguageFromCookie(t *testing.T) {
	router := gin.New()
	router.Use(ginapi.Language(ginapi.LanguageConfig{Supported: []string{"en", "ja", "ko"}}))
	router.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, ginapi.GetLanguage(c)) })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.AddCookie(&http.Cookie{Name: "lang", Value: "KO"})
	req.Header.Set("Accept-Language", "ja")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if got := w.Body.String(); got != "ko" {
		t.Errorf("got %q: the saved cookie outranks Accept-Language", got)
	}
}

func TestLanguagePrecedence(t *testing.T) {
	// query > path > cookie > Accept-Language > default
	router := gin.New()
	router.Use(ginapi.Language(ginapi.LanguageConfig{Supported: []string{"en", "ja", "ko", "zh"}}))
	router.GET("/:lang/x", func(c *gin.Context) { c.String(http.StatusOK, ginapi.GetLanguage(c)) })

	req := httptest.NewRequest(http.MethodGet, "/ja/x?lang=zh", nil)
	req.AddCookie(&http.Cookie{Name: "lang", Value: "ko"})
	req.Header.Set("Accept-Language", "en")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if got := w.Body.String(); got != "zh" {
		t.Errorf("got %q, want zh from the query parameter", got)
	}
}

func TestUnsupportedValuesFallThrough(t *testing.T) {
	router := gin.New()
	router.Use(ginapi.Language(ginapi.LanguageConfig{Supported: []string{"en", "ja"}}))
	router.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, ginapi.GetLanguage(c)) })

	req := httptest.NewRequest(http.MethodGet, "/x?lang=de", nil)
	req.AddCookie(&http.Cookie{Name: "lang", Value: "fr"})
	req.Header.Set("Accept-Language", "ja;q=0.8,de;q=0.9")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if got := w.Body.String(); got != "ja" {
		t.Errorf("got %q: unsupported query and cookie values fall through to the header", got)
	}
}

func TestExtractLanguageFromPath(t *testing.T) {
	cases := map[string]string{
		"/ja/galleries": "ja",
		"/en":           "en",
		"/zho/x":        "zho",
		"/galleries":    "", // only a 2-3 character first segment is a language
		"/":             "",
		"":              "",
	}
	for path, want := range cases {
		if got := ginapi.ExtractLanguageFromPath(path); got != want {
			t.Errorf("ExtractLanguageFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestBuildSupportedMap(t *testing.T) {
	m := ginapi.BuildSupportedMap([]string{"EN", "Ja"})
	if _, ok := m["en"]; !ok {
		t.Error("want en lowercased into the map")
	}
	if _, ok := m["ja"]; !ok {
		t.Error("want ja lowercased into the map")
	}
}

func TestParseAcceptLanguageQValues(t *testing.T) {
	supported := ginapi.BuildSupportedMap([]string{"en", "ja", "ko"})
	if got := ginapi.ParseAcceptLanguage("ko;q=0.2,ja;q=0.9,en;q=0.5", supported); got != "ja" {
		t.Errorf("got %q, want the highest q-value", got)
	}
	if got := ginapi.ParseAcceptLanguage("de,fr", supported); got != "" {
		t.Errorf("got %q, want empty when nothing is supported", got)
	}
	if got := ginapi.ParseAcceptLanguage("ja-JP", supported); got != "ja" {
		t.Errorf("got %q, want the base subtag", got)
	}
}

func TestSetLanguageCookie(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	ginapi.SetLanguageCookie(c, "ja")
	if got := w.Header().Get("Set-Cookie"); got == "" {
		t.Fatal("want a Set-Cookie header")
	}
}
