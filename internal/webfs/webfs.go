// Package webfs exposes the embedded frontend assets.
package webfs

import "embed"

//go:embed all:web
var FS embed.FS
