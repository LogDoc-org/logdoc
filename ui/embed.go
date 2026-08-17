// Package ui embeds the built web UI (go:embed).
// Build the frontend before go build: make ui (npm run build → ui/dist).
package ui

import "embed"

//go:embed all:dist
var Dist embed.FS
