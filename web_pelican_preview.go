package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

const pelicanPreviewCSP = "default-src 'none'; sandbox allow-scripts; base-uri 'none'; frame-ancestors 'self'; form-action 'none'; object-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; connect-src 'none'; frame-src 'none'; worker-src 'none';"

func registerPelicanPreviewRoutes(router *gin.Engine, rt *appRuntime, authRequired gin.HandlerFunc) {
	router.GET("/api/pelican/preview/:sessionId", authRequired, func(c *gin.Context) {
		if rt.pelicanTestService == nil {
			c.Status(http.StatusNotFound)
			return
		}
		html, ok := rt.pelicanTestService.PreviewHTML(c.Param("sessionId"), c.Query("token"))
		if !ok {
			c.Status(http.StatusNotFound)
			return
		}
		c.Writer.Header().Del("X-Frame-Options")
		c.Header("Content-Security-Policy", pelicanPreviewCSP)
		c.Header("Cache-Control", "no-store")
		c.Header("Referrer-Policy", "no-referrer")
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(html))
	})
}
