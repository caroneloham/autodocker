package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestProxyOriginProtection(t *testing.T) {
	r := gin.New()
	r.Use(ProxyOriginProtection())
	r.Any("/view/app", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	for _, tc := range []struct {
		origin, site, referrer string
		status                 int
	}{
		{"https://autodocker.example", "same-origin", "", 204},
		{"", "none", "", 204},
		{"https://other.example", "cross-site", "", 403},
		{"", "same-site", "", 403},
		{"", "", "https://other.example/form", 403},
		{"", "", "https://autodocker.example/form", 204},
	} {
		request := httptest.NewRequest(http.MethodPost, "https://autodocker.example/view/app", nil)
		if tc.origin != "" {
			request.Header.Set("Origin", tc.origin)
		}
		if tc.site != "" {
			request.Header.Set("Sec-Fetch-Site", tc.site)
		}
		if tc.referrer != "" {
			request.Header.Set("Referer", tc.referrer)
		}
		out := httptest.NewRecorder()
		r.ServeHTTP(out, request)
		if out.Code != tc.status {
			t.Fatalf("origin=%q site=%q referrer=%q status=%d", tc.origin, tc.site, tc.referrer, out.Code)
		}
	}
}
