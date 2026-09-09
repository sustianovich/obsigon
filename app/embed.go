// Package webui contiene la interfaz web distribuida dentro del binario.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
)

// bundled conserva la estructura usada durante el desarrollo.
//
//go:embed static/** templates/index.html
var bundled embed.FS

// Handler sirve la UI empotrada o, si overrideDir no está vacío, los archivos
// de ese directorio. El override debe contener static/ y templates/index.html.
func Handler(overrideDir string) (http.Handler, error) {
	assets := fs.FS(bundled)
	if overrideDir != "" {
		assets = os.DirFS(filepath.Clean(overrideDir))
	}
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	staticHandler := http.StripPrefix("/static/", http.FileServer(http.FS(static)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// La UI va empotrada en el binario: cada build la sustituye entera,
		// y los módulos JS importados entre sí (api.js, chat.js, state.js...)
		// no llevan query de versión. Sin esta cabecera el navegador puede
		// quedarse con una mezcla de archivos de builds distintas (o
		// distintos del binario que responde ahora mismo) y romper la UI en
		// silencio tras actualizar. Al ser una app local, no cachear estos
		// archivos no tiene coste apreciable.
		w.Header().Set("Cache-Control", "no-store")
		switch {
		case r.URL.Path == "/":
			data, readErr := fs.ReadFile(assets, "templates/index.html")
			if readErr != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(data)
		case len(r.URL.Path) > len("/static/") && r.URL.Path[:len("/static/")] == "/static/":
			staticHandler.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}), nil
}
