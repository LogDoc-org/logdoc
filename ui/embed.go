// Package ui — встроенная сборка веб-интерфейса (go:embed).
// Перед go build необходимо собрать фронтенд: make ui (npm run build → ui/dist).
package ui

import "embed"

//go:embed all:dist
var Dist embed.FS
