package handlers

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"autodocker-go/middleware"

	"github.com/gin-gonic/gin"
)

// RegisterProxyRoutes registers the reverse proxy for container ports.
func RegisterProxyRoutes(r *gin.Engine) {
	proxy := r.Group("/api/proxy")
	proxy.Use(middleware.LoginRequired())
	{
		proxy.Any("/:port/*path", handleProxy)
		proxy.Any("/:port", handleProxy)
	}
	portView := r.Group("/api/port-view")
	portView.Use(middleware.LoginRequired())
	{
		portView.Any("/:port/*path", handleProxy)
		portView.Any("/:port", handleProxy)
	}
	tokenView := r.Group("/view")
	tokenView.Use(middleware.LoginRequired())
	{
		tokenView.Any("/:token/*path", handleProxy)
		tokenView.Any("/:token", handleProxy)
	}
}

func handleProxy(c *gin.Context) {
	port := c.Param("port")
	token := c.Param("token")
	if port == "" && token != "" {
		decoded, ok := decodeProxyPortToken(token)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Port invalide"})
			return
		}
		port = decoded
	}
	if !isValidProxyPort(port) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Port invalide"})
		return
	}

	path := c.Param("path")
	if path == "" {
		path = "/"
	}

	targetHost := proxyTargetHost(c.Request.Host)
	target, _ := url.Parse(fmt.Sprintf("http://%s:%s", targetHost, port))
	prefix := proxyPrefix(c.Request.URL.Path, port, token)
	originalHost := c.Request.Host

	stripProxyFrameHeaders(c.Writer.Header())

	proxy := httputil.NewSingleHostReverseProxy(target)

	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = path
		req.URL.RawQuery = c.Request.URL.RawQuery
		req.Host = target.Host
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("X-Forwarded-Host", originalHost)
		req.Header.Set("X-Forwarded-Prefix", prefix)
		req.Header.Set("X-Forwarded-Proto", forwardedProto(c.Request))
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		stripProxyFrameHeaders(resp.Header)
		rewriteProxyLocation(resp.Header, port, prefix, originalHost)
		rewriteProxyCookiePaths(resp.Header, prefix)

		if !shouldRewriteProxyBody(resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding")) {
			return nil
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()

		rewritten := rewriteProxyBody(body, port, prefix, originalHost)
		resp.Body = io.NopCloser(bytes.NewReader(rewritten))
		resp.ContentLength = int64(len(rewritten))
		resp.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
		return nil
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, fmt.Sprintf("Proxy indisponible sur le port %s: %v", port, err), http.StatusBadGateway)
	}

	proxy.ServeHTTP(c.Writer, c.Request)
}

func proxyPrefix(requestPath, port, token string) string {
	if strings.HasPrefix(requestPath, "/view/") && token != "" {
		return "/view/" + token
	}
	if strings.HasPrefix(requestPath, "/api/port-view/") {
		return "/api/port-view/" + port
	}
	return "/api/proxy/" + port
}

func decodeProxyPortToken(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	var b strings.Builder
	for _, ch := range token {
		if ch < 'a' || ch > 'j' {
			return "", false
		}
		b.WriteByte(byte('0' + ch - 'a'))
	}
	port := b.String()
	return port, isValidProxyPort(port)
}

func isValidProxyPort(port string) bool {
	p, err := strconv.Atoi(port)
	return err == nil && p > 0 && p <= 65535
}

func forwardedProto(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	return "http"
}

func stripProxyFrameHeaders(h http.Header) {
	h.Del("X-Frame-Options")
	h.Del("Content-Security-Policy")
	h.Del("Content-Security-Policy-Report-Only")
}

func rewriteProxyLocation(h http.Header, port, prefix, originalHost string) {
	location := h.Get("Location")
	if location == "" {
		return
	}

	if strings.HasPrefix(location, "/") && !strings.HasPrefix(location, "//") {
		h.Set("Location", prefix+location)
		return
	}

	if strings.HasPrefix(location, "//") {
		location = "http:" + location
	}

	u, err := url.Parse(location)
	if err != nil || u.Host == "" {
		return
	}
	if isProxyTargetHost(u.Hostname(), u.Port(), port, originalHost) {
		h.Set("Location", prefix+u.RequestURI())
	}
}

func rewriteProxyCookiePaths(h http.Header, prefix string) {
	cookies := h.Values("Set-Cookie")
	if len(cookies) == 0 {
		return
	}

	h.Del("Set-Cookie")
	pathRe := regexp.MustCompile(`(?i);\s*Path=[^;]*`)
	domainRe := regexp.MustCompile(`(?i);\s*Domain=[^;]*`)
	for _, cookie := range cookies {
		cookie = domainRe.ReplaceAllString(cookie, "")
		if pathRe.MatchString(cookie) {
			cookie = pathRe.ReplaceAllString(cookie, "; Path="+prefix)
		} else {
			cookie += "; Path=" + prefix
		}
		h.Add("Set-Cookie", cookie)
	}
}

func shouldRewriteProxyBody(contentType, contentEncoding string) bool {
	if contentEncoding != "" && !strings.EqualFold(contentEncoding, "identity") {
		return false
	}

	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	}

	switch mediaType {
	case "text/html", "text/css", "text/javascript", "application/javascript", "application/x-javascript":
		return true
	default:
		return false
	}
}

func rewriteProxyBody(body []byte, port, prefix, originalHost string) []byte {
	s := string(body)
	host := hostWithoutPort(originalHost)

	for _, h := range []string{"localhost", "127.0.0.1", "[::1]", host, proxyTargetHost(originalHost)} {
		if h == "" {
			continue
		}
		for _, scheme := range []string{"http", "https"} {
			s = strings.ReplaceAll(s, fmt.Sprintf("%s://%s:%s", scheme, h, port), prefix)
		}
		s = strings.ReplaceAll(s, fmt.Sprintf("//%s:%s", h, port), prefix)
	}

	replacements := []struct {
		re *regexp.Regexp
	}{
		{regexp.MustCompile(`(?i)(\b(?:href|src|action|poster|data-src)=["'])/([^"']*)`)},
		{regexp.MustCompile(`(?i)(url\(\s*['"]?)/([^'")]*)`)},
		{regexp.MustCompile(`(?i)(\bcontent=["'][^"']*url=)/([^"']*)`)},
	}
	for _, r := range replacements {
		s = rewriteRootProxyRefs(s, r.re, prefix)
	}
	return []byte(s)
}

func rewriteRootProxyRefs(s string, re *regexp.Regexp, prefix string) string {
	return re.ReplaceAllStringFunc(s, func(match string) string {
		parts := re.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		path := parts[2]
		lowerPath := strings.ToLower(path)
		if strings.HasPrefix(path, "/") || strings.HasPrefix(lowerPath, "api/proxy/") || strings.HasPrefix(lowerPath, "api/port-view/") || strings.HasPrefix(lowerPath, "view/") {
			return match
		}
		return parts[1] + prefix + "/" + path
	})
}

func isProxyTargetHost(hostname, actualPort, expectedPort, originalHost string) bool {
	if actualPort != expectedPort {
		return false
	}
	host := strings.ToLower(hostname)
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	original := strings.ToLower(hostWithoutPort(originalHost))
	target := strings.ToLower(proxyTargetHost(originalHost))
	return host != "" && (host == original || host == target)
}

func hostWithoutPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(host, "[]")
}

func proxyTargetHost(requestHost string) string {
	host := strings.ToLower(hostWithoutPort(requestHost))
	if host != "" && host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return host
	}

	if ip := firstNonLoopbackIPv4(); ip != "" {
		return ip
	}
	return "127.0.0.1"
}

func firstNonLoopbackIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		ip := ipNet.IP.To4()
		if ip == nil {
			continue
		}
		ipStr := ip.String()
		if strings.HasPrefix(ipStr, "169.254.") || strings.HasPrefix(ipStr, "192.168.56.") {
			continue
		}
		return ipStr
	}
	return ""
}
