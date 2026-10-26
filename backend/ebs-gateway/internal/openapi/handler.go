// Package openapi serves the offline-generated public Gateway contract.
package openapi

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed openapi.json
var document []byte

func Serve(c *gin.Context) {
	c.Header("Cache-Control", "no-cache")
	c.Data(http.StatusOK, "application/json; charset=utf-8", document)
}
