package web

import "mime"

// On Windows, Go seeds its mime table from HKEY_CLASSES_ROOT and those
// values win over its built-in ones, so a machine with .css registered as
// text/plain serves a stylesheet no browser will apply. AddExtensionType
// runs that OS load before it writes, so these land on top of it.
func init() {
	for ext, typ := range map[string]string{
		".css":  "text/css; charset=utf-8",
		".js":   "text/javascript; charset=utf-8",
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".gif":  "image/gif",
		".webp": "image/webp",
		".mp4":  "video/mp4",
		".webm": "video/webm",
	} {
		if err := mime.AddExtensionType(ext, typ); err != nil {
			panic(err)
		}
	}
}
