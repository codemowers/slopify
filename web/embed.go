// Package web holds the browser app served by the frontend.
package web

import "embed"

//go:embed index.html app.js media.js network.js output.html output.js style.css sw.js manifest.webmanifest icon.svg
var FS embed.FS
