package web

import "strings"

// LocalPath makes p safe to use as a same-site redirect target: browsers read
// a leading "//" or "/\" as another host, so runs of leading slashes and
// backslashes collapse to one "/", and anything that isn't a path becomes "/".
func LocalPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\r\n") {
		return "/"
	}
	return "/" + strings.TrimLeft(p, "/\\")
}
