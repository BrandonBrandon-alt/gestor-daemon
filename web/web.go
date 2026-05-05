// Package web provides static assets and HTML templates for the UI.
package web

import (
	_ "embed"
)

//go:embed index.html
var IndexHTML []byte
