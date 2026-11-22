package server

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/labstack/echo/v5"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	telegramRelayAPIPrefix  = "/telegram/bot"
	telegramRelayFilePrefix = "/telegram/file/bot"
	telegramRelayUpstream   = "api.telegram.org"
)

type telegramRelayOriginalPathKey struct{}

type telegramRelayHandler struct {
	botToken    string
	clientToken string
	transport   http.RoundTripper
}

// newTelegramRelayFromEnv enables the relay only when both credentials are
// configured. The real Telegram token remains on the GoModel host; Hermes uses
// a separate random client token that is safe to expose only to this relay.
func newTelegramRelayFromEnv() http.Handler {
	botToken := strings.TrimSpace(os.Getenv("TELEGRAM_RELAY_BOT_TOKEN"))
	clientToken := strings.TrimSpace(os.Getenv("TELEGRAM_RELAY_CLIENT_TOKEN"))
	if botToken == "" || clientToken == "" || botToken == clientToken {
		return nil
	}
	return newTelegramRelayHandlerWithTransport(botToken, clientToken, newTelegramRelayTransport())
}

func newTelegramRelayHandlerWithTransport(botToken, clientToken string, transport http.RoundTripper) *telegramRelayHandler {
	if transport == nil {
		transport = newTelegramRelayTransport()
	}
	return &telegramRelayHandler{
		botToken:    strings.TrimSpace(botToken),
		clientToken: strings.TrimSpace(clientToken),
		transport:   transport,
	}
}

// Keep a small connection pool: this relay serves one bot on a low-memory gateway.
func newTelegramRelayTransport() http.RoundTripper {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   2,
		MaxConnsPerHost:       4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 65 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}


// telegramRelayWriteDeadlineMiddleware removes the gateway's short absolute
// write deadline for Telegram routes. getUpdates may legitimately wait up to
// 50 seconds before the upstream responds; the outbound transport still has a
// finite response-header timeout and connection limits.
func telegramRelayWriteDeadlineMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			controller := http.NewResponseController(c.Response())
			if err := controller.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return err
			}
			return next(c)
		}
	}
}

func (h *telegramRelayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.botToken == "" || h.clientToken == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost:
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	upstreamPath, ok := telegramRelayUpstreamPath(telegramRelayOriginalPath(r), h.botToken, h.clientToken)
	if !ok {
		writeTelegramRelayError(w, http.StatusNotFound, "not found")
		return
	}

	target := &url.URL{
		Scheme:   "https",
		Host:     telegramRelayUpstream,
		Path:     upstreamPath,
		RawQuery: r.URL.RawQuery,
	}
	out := r.Clone(r.Context())
	out.URL = target
	out.Host = target.Host
	out.RequestURI = ""
	out.Header = r.Header.Clone()
	removeTelegramHopByHopHeaders(out.Header)
	out.Header.Del("Authorization")
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("X-Forwarded-For")
	out.Header.Del("X-Forwarded-Host")
	out.Header.Del("X-Forwarded-Proto")

	response, err := h.transport.RoundTrip(out)
	if err != nil {
		// Transport errors may include the upstream URL and real bot token.
		writeTelegramRelayError(w, http.StatusBadGateway, "Telegram API upstream unavailable")
		return
	}
	defer response.Body.Close()

	copyTelegramRelayHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, response.Body)
	}
}

func telegramRelayOriginalPath(r *http.Request) string {
	if r != nil {
		if original, ok := r.Context().Value(telegramRelayOriginalPathKey{}).(string); ok && original != "" {
			return original
		}
		if r.URL != nil {
			return r.URL.Path
		}
	}
	return ""
}

func telegramRelayUpstreamPath(requestPath, botToken, clientToken string) (string, bool) {
	if strings.HasPrefix(requestPath, telegramRelayFilePrefix) {
		remainder := strings.TrimPrefix(requestPath, telegramRelayFilePrefix)
		token, filePath, found := strings.Cut(remainder, "/")
		if !found || !telegramRelayTokenMatches(token, clientToken) || !validTelegramFilePath(filePath) {
			return "", false
		}
		return "/file/bot" + botToken + "/" + filePath, true
	}

	if strings.HasPrefix(requestPath, telegramRelayAPIPrefix) {
		remainder := strings.TrimPrefix(requestPath, telegramRelayAPIPrefix)
		token, method, found := strings.Cut(remainder, "/")
		if !found || !telegramRelayTokenMatches(token, clientToken) || !validTelegramMethod(method) {
			return "", false
		}
		return "/bot" + botToken + "/" + method, true
	}
	return "", false
}

func telegramRelayTokenMatches(candidate, expected string) bool {
	if candidate == "" || expected == "" || len(candidate) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(expected)) == 1
}

func validTelegramMethod(method string) bool {
	if method == "" {
		return false
	}
	for i := 0; i < len(method); i++ {
		ch := method[i]
		isLetter := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
		isDigit := ch >= '0' && ch <= '9'
		if !isLetter && !(i > 0 && (isDigit || ch == '_')) {
			return false
		}
	}
	return true
}

func validTelegramFilePath(filePath string) bool {
	if filePath == "" || strings.HasPrefix(filePath, "/") || strings.ContainsAny(filePath, "\\\x00") {
		return false
	}
	for _, segment := range strings.Split(filePath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func removeTelegramHopByHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				header.Del(name)
			}
		}
	}
	for _, name := range []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Proxy-Connection", "TE", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		header.Del(name)
	}
}

func copyTelegramRelayHeaders(dst, src http.Header) {
	for name, values := range src {
		if isTelegramHopByHopHeader(name) {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func isTelegramHopByHopHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}

func writeTelegramRelayError(w http.ResponseWriter, status int, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "{\"ok\":false,\"error_code\":%d,\"description\":%q}", status, description)
}

// Redact the client token from logs while retaining the original path in request context.
func redactTelegramRelayPath(requestPath string) string {
	for _, prefix := range []string{telegramRelayFilePrefix, telegramRelayAPIPrefix} {
		if !strings.HasPrefix(requestPath, prefix) {
			continue
		}
		remainder := strings.TrimPrefix(requestPath, prefix)
		if remainder == "" || strings.HasPrefix(remainder, "/") {
			return requestPath
		}
		if slash := strings.IndexByte(remainder, '/'); slash >= 0 {
			return prefix + "REDACTED" + remainder[slash:]
		}
		return prefix + "REDACTED"
	}
	return requestPath
}

func redactTelegramRelayURI(requestURI string) string {
	path, query, hasQuery := strings.Cut(requestURI, "?")
	path = redactTelegramRelayPath(path)
	if !hasQuery {
		return path
	}
	return path + "?" + query
}

func isTelegramRelayPath(requestPath string) bool {
	return strings.HasPrefix(requestPath, telegramRelayAPIPrefix) ||
		strings.HasPrefix(requestPath, telegramRelayFilePrefix)
}
