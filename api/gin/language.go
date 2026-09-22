package ginapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	// LanguageCookieName is the default cookie name for language preference
	LanguageCookieName = "lang"
	// LanguageCookieMaxAge is 1 year in seconds
	LanguageCookieMaxAge = 31536000
)

// LanguageConfig configures the language detection middleware.
type LanguageConfig struct {
	// Supported languages (e.g., []string{"en", "ja", "ko", "zh"})
	Supported []string
	// Default language if none detected (defaults to "en")
	Default string
	// QueryParam to check for language override (defaults to "lang")
	QueryParam string
	// CookieName to check for language preference (defaults to "lang")
	CookieName string
}

// Language returns middleware that detects user language from:
// 1. Query parameter (?lang=ja) - for API routes
// 2. URL path prefix (/ja/...) - for frontend routes
// 3. Cookie (user's saved preference)
// 4. Accept-Language header with q-value parsing
// 5. Default language
//
// The detected language is stored in gin context and retrieved via GetLanguage(c).
// The Content-Language header is set on the response.
func Language(cfg LanguageConfig) gin.HandlerFunc {
	// Build supported language map for fast lookup
	supportedMap := make(map[string]struct{}, len(cfg.Supported))
	for _, lang := range cfg.Supported {
		supportedMap[strings.ToLower(lang)] = struct{}{}
	}
	if len(supportedMap) == 0 {
		supportedMap["en"] = struct{}{}
	}

	// Normalize defaults
	defaultLang := strings.ToLower(strings.TrimSpace(cfg.Default))
	if defaultLang == "" {
		defaultLang = "en"
	}

	queryParam := cfg.QueryParam
	if queryParam == "" {
		queryParam = "lang"
	}

	cookieName := cfg.CookieName
	if cookieName == "" {
		cookieName = "lang"
	}

	return func(c *gin.Context) {
		lang := resolveLanguage(c, supportedMap, defaultLang, queryParam, cookieName)

		// Store in gin context (use GetLanguage(c) to retrieve)
		c.Set("language", lang)

		// Set response header
		c.Header("Content-Language", lang)

		c.Next()
	}
}

// resolveLanguage determines the best language from available sources.
func resolveLanguage(c *gin.Context, supported map[string]struct{}, fallback, queryParam, cookieName string) string {
	if c == nil || c.Request == nil {
		return fallback
	}

	// 1. Check query parameter (for API routes like /api/v1/videos?lang=ja)
	if lang := strings.ToLower(strings.TrimSpace(c.Query(queryParam))); lang != "" {
		if _, ok := supported[lang]; ok {
			return lang
		}
	}

	// 2. Check URL path prefix (for frontend routes like /ja/videos)
	if lang := extractLanguageFromPath(c.Request.URL.Path); lang != "" {
		if _, ok := supported[lang]; ok {
			return lang
		}
	}

	// 3. Check cookie (user's saved preference)
	if cookieName != "" {
		if lang, err := c.Cookie(cookieName); err == nil && lang != "" {
			lang = strings.ToLower(strings.TrimSpace(lang))
			if _, ok := supported[lang]; ok {
				return lang
			}
		}
	}

	// 4. Check Accept-Language header
	if header := c.GetHeader("Accept-Language"); header != "" {
		if lang := ParseAcceptLanguage(header, supported); lang != "" {
			return lang
		}
	}

	return fallback
}

// extractLanguageFromPath extracts a 2-3 character language code from URL path prefix.
// e.g., "/ja/galleries" -> "ja"
func extractLanguageFromPath(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) == 0 {
		return ""
	}
	first := parts[0]
	if len(first) == 2 || len(first) == 3 {
		return strings.ToLower(first)
	}
	return ""
}

// ParseAcceptLanguage parses the Accept-Language header and returns the best
// supported language based on q-values. Exported for use by redirect middleware.
func ParseAcceptLanguage(header string, supported map[string]struct{}) string {
	type candidate struct {
		lang string
		q    float64
	}

	var candidates []candidate
	parts := strings.Split(header, ",")

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		lang := part
		q := 1.0

		// Parse q-value if present (e.g., "en-US;q=0.9")
		if idx := strings.Index(part, ";"); idx >= 0 {
			lang = part[:idx]
			param := strings.TrimSpace(part[idx+1:])
			if strings.HasPrefix(param, "q=") {
				if v, err := strconv.ParseFloat(strings.TrimPrefix(param, "q="), 64); err == nil {
					q = v
				}
			}
		}

		// Extract base language (e.g., "en-US" -> "en")
		if hyphen := strings.Index(lang, "-"); hyphen >= 0 {
			lang = lang[:hyphen]
		}
		lang = strings.ToLower(strings.TrimSpace(lang))

		if lang == "" {
			continue
		}

		candidates = append(candidates, candidate{lang: lang, q: q})
	}

	// Find highest q-value language that's supported
	var bestLang string
	var bestQ float64 = -1

	for _, c := range candidates {
		if _, ok := supported[c.lang]; ok && c.q > bestQ {
			bestLang = c.lang
			bestQ = c.q
		}
	}

	return bestLang
}

// GetLanguage retrieves the detected language from the gin context.
// Returns "en" as fallback if not set.
func GetLanguage(c *gin.Context) string {
	if c == nil {
		return "en"
	}

	if lang, exists := c.Get("language"); exists {
		if s, ok := lang.(string); ok && s != "" {
			return s
		}
	}

	return "en"
}

// BuildSupportedMap creates a map of supported languages for fast lookup.
// Useful for redirect middleware that needs to check language validity.
func BuildSupportedMap(languages []string) map[string]struct{} {
	m := make(map[string]struct{}, len(languages))
	for _, lang := range languages {
		m[strings.ToLower(lang)] = struct{}{}
	}
	return m
}

// ExtractLanguageFromPath extracts a 2-3 character language code from URL path prefix.
// e.g., "/ja/galleries" -> "ja", "/galleries" -> ""
// Exported for use by redirect middleware.
func ExtractLanguageFromPath(path string) string {
	return extractLanguageFromPath(path)
}

// LanguageRedirectConfig configures language redirect behavior for NoRoute handlers.
type LanguageRedirectConfig struct {
	// Supported languages (e.g., []string{"en", "ja", "ko", "zh"})
	Supported []string
	// Default language if none detected (defaults to "en")
	Default string
}

// HandleLanguageRedirect checks if a language redirect is needed and performs it.
// Returns true if a redirect was performed (caller should return early).
// Returns false if no redirect needed (caller should continue to serve the page).
//
// This is designed to be called from a NoRoute handler:
//
//	r.NoRoute(func(c *gin.Context) {
//	    if middleware.HandleLanguageRedirect(c, cfg) {
//	        return // redirect was performed
//	    }
//	    // serve SPA
//	    serveIndexHTML(c)
//	})
//
// Behavior:
//   - If URL has a valid language prefix (e.g., /en/videos): set cookie, return false
//   - If URL has NO language prefix (e.g., /videos): redirect to prefixed URL, return true
func HandleLanguageRedirect(c *gin.Context, cfg LanguageRedirectConfig) bool {
	if len(cfg.Supported) == 0 {
		return false
	}

	supportedMap := BuildSupportedMap(cfg.Supported)
	defaultLang := strings.ToLower(strings.TrimSpace(cfg.Default))
	if defaultLang == "" {
		defaultLang = "en"
	}

	path := c.Request.URL.Path

	// Check if URL already has a language prefix
	langFromPath := extractLanguageFromPath(path)
	if langFromPath != "" {
		if _, ok := supportedMap[langFromPath]; ok {
			// Valid language prefix - set cookie and continue
			SetLanguageCookie(c, langFromPath)
			return false
		}
		// Invalid language prefix - fall through to redirect
	}

	// No valid language prefix - determine preferred language and redirect
	preferredLang := DetectPreferredLanguage(c, supportedMap, defaultLang)

	// Build redirect URL with language prefix
	redirectURL := "/" + preferredLang + path
	if c.Request.URL.RawQuery != "" {
		redirectURL += "?" + c.Request.URL.RawQuery
	}

	c.Redirect(http.StatusFound, redirectURL)
	c.Abort()
	return true
}

// DetectPreferredLanguage determines user's preferred language.
// Priority: cookie → Accept-Language → default
func DetectPreferredLanguage(c *gin.Context, supportedMap map[string]struct{}, defaultLang string) string {
	// 1. Check cookie (user's saved preference)
	if lang, err := c.Cookie(LanguageCookieName); err == nil && lang != "" {
		lang = strings.ToLower(strings.TrimSpace(lang))
		if _, ok := supportedMap[lang]; ok {
			return lang
		}
	}

	// 2. Check Accept-Language header
	if header := c.GetHeader("Accept-Language"); header != "" {
		if lang := ParseAcceptLanguage(header, supportedMap); lang != "" {
			return lang
		}
	}

	// 3. Fall back to default
	return defaultLang
}

// SetLanguageCookie sets the language preference cookie (1 year, SameSite=Lax).
func SetLanguageCookie(c *gin.Context, lang string) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(LanguageCookieName, lang, LanguageCookieMaxAge, "/", "", false, false)
}
