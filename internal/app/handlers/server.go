package handlers

import (
	"net/http"

	"github.com/thushan/olla/internal/core/constants"
)

func (a *Application) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.logger.Info("HTTP request",
			"method", r.Method,
			"path", r.URL.Path,
			"remote", r.RemoteAddr,
			"query", r.URL.RawQuery,
			"request_uri", r.RequestURI,
			"content_type", r.Header.Get(constants.HeaderContentType),
			"content_length", r.ContentLength,
			"host", r.Host,
			"referer", r.Referer(),
			"user_agent", r.UserAgent())
		next.ServeHTTP(w, r)
	})
}
