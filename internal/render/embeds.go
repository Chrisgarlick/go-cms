package render

import (
	"fmt"
	"html"
	"html/template"
	"net/url"
	"regexp"
	"strings"
)

// Embeds never fetch provider oEmbed HTML (no SSRF, no third-party markup in
// our pages). Allow-listed URLs become a link that a tiny base-theme script
// swaps for a sandboxed iframe on click; without JavaScript it stays a link.

var (
	ytID      = regexp.MustCompile(`^[A-Za-z0-9_-]{6,20}$`)
	vimeoID   = regexp.MustCompile(`^[0-9]{3,12}$`)
	loomID    = regexp.MustCompile(`^[a-f0-9]{16,64}$`)
	spotifyID = regexp.MustCompile(`^[A-Za-z0-9]{10,40}$`)
)

type embedTarget struct {
	Provider string
	Src      string
	Ratio    string // CSS class suffix: 16x9 | 4x3 | auto
}

// EmbedTarget maps a public URL to an embeddable iframe source.
func EmbedTarget(raw string) (embedTarget, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" {
		return embedTarget{}, false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch host {
	case "youtube.com", "m.youtube.com":
		id := u.Query().Get("v")
		if len(parts) == 2 && (parts[0] == "shorts" || parts[0] == "embed" || parts[0] == "live") {
			id = parts[1]
		}
		if ytID.MatchString(id) {
			return embedTarget{"YouTube", "https://www.youtube-nocookie.com/embed/" + id + "?autoplay=1", "16x9"}, true
		}
	case "youtu.be":
		if len(parts) == 1 && ytID.MatchString(parts[0]) {
			return embedTarget{"YouTube", "https://www.youtube-nocookie.com/embed/" + parts[0] + "?autoplay=1", "16x9"}, true
		}
	case "vimeo.com":
		if len(parts) >= 1 && vimeoID.MatchString(parts[len(parts)-1]) {
			return embedTarget{"Vimeo", "https://player.vimeo.com/video/" + parts[len(parts)-1] + "?autoplay=1&dnt=1", "16x9"}, true
		}
	case "loom.com":
		if len(parts) == 2 && parts[0] == "share" && loomID.MatchString(parts[1]) {
			return embedTarget{"Loom", "https://www.loom.com/embed/" + parts[1], "16x9"}, true
		}
	case "open.spotify.com":
		if len(parts) == 2 && spotifyID.MatchString(parts[1]) {
			switch parts[0] {
			case "track", "album", "playlist", "episode", "show":
				return embedTarget{"Spotify", "https://open.spotify.com/embed/" + parts[0] + "/" + parts[1], "auto"}, true
			}
		}
	case "codepen.io":
		if len(parts) == 3 && parts[1] == "pen" && ytID.MatchString(parts[2]) {
			return embedTarget{"CodePen", "https://codepen.io/" + url.PathEscape(parts[0]) + "/embed/" + parts[2] + "?default-tab=result", "4x3"}, true
		}
	}
	return embedTarget{}, false
}

func embedFacade(raw, title string) template.HTML {
	t, ok := EmbedTarget(raw)
	if !ok {
		if s, ok := safeLink(raw); ok {
			return template.HTML(fmt.Sprintf(`<p class="embed-link"><a href="%s" rel="noopener">%s</a></p>`,
				html.EscapeString(s), html.EscapeString(firstNonEmpty(title, s))))
		}
		return ""
	}
	label := firstNonEmpty(title, t.Provider+" embed")
	return template.HTML(fmt.Sprintf(
		`<div class="embed embed-%s" data-embed-src="%s" data-embed-title="%s"><a class="embed-facade" href="%s" rel="noopener"><span class="embed-play" aria-hidden="true"></span><span class="embed-label">%s</span><span class="embed-note">Plays from %s</span></a></div>`,
		t.Ratio, html.EscapeString(t.Src), html.EscapeString(label), html.EscapeString(raw), html.EscapeString(label), t.Provider))
}

func safeLink(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", false
	}
	return u.String(), true
}

// EmbedFrameSources lists hosts the public CSP must allow in frame-src.
var EmbedFrameSources = []string{"https://www.youtube-nocookie.com", "https://player.vimeo.com", "https://www.loom.com",
	"https://open.spotify.com", "https://codepen.io"}
