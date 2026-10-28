package main

import (
	"github.com/codex2api/api"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

func configureRequestBodyMiddleware(r *gin.Engine, maxSize int64) {
	// CORS headers must also be present on early body-size/decompression errors.
	r.Use(api.CORSMiddleware())
	r.Use(security.RequestSizeLimiter(maxSize))
	r.Use(security.RequestBodyDecompressor(maxSize))
	r.Use(api.BodyCacheMiddleware())
}
