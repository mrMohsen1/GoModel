package server

import (
	"context"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/labstack/echo/v5"
)

// redactSensitiveRequestURI keeps query parameters available to handlers while
// preventing the outer access logger from persisting OAuth credentials and
// transaction secrets. It applies generically so extensions do not need to
// expose provider-specific callback paths to Core.
func redactSensitiveRequestURI() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			request := c.Request()
			if isTelegramRelayPath(request.URL.Path) {
				originalPath := request.URL.Path
				request = request.WithContext(context.WithValue(request.Context(), telegramRelayOriginalPathKey{}, originalPath))
				request.URL = request.URL.Clone()
				request.URL.Path = redactTelegramRelayPath(originalPath)
				request.URL.RawPath = ""
			}
			request.RequestURI = redactTelegramRelayURI(core.RedactSensitiveURLQuery(request.RequestURI))
			c.SetRequest(request)
			return next(c)
		}
	}
}
