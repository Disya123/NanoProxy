package ui

import (
	"html/template"
	"io"
	"io/fs"
	"time"
)

// Templates is the parsed *template.Template root for admin pages.
// Each content page defines block {{define "content"}}; the layout provides
// the surrounding chrome and calls {{template "content" .}}.
var Templates = func() *template.Template {
	funcs := template.FuncMap{
		"fmtTime": func(ms int64) string {
			return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
		},
		"fmtUSD": func(v float64) string {
			if v == 0 {
				return "$0.00"
			}
			if v < 0.01 {
				return "$" + formatFloat(v, 6)
			}
			return "$" + formatFloat(v, 2)
		},
		"fmtNum": func(v int64) string { return formatIntGrouped(v) },
		"statusClass": func(code int) string {
			switch {
			case code >= 500:
				return "badge-bad"
			case code >= 400:
				return "badge-warn"
			case code >= 300:
				return "badge-warn"
			default:
				return "badge-ok"
			}
		},
		"isErr": func(code int) bool { return code >= 400 },
		"cacheRate": func(cached, prompt int) string {
			if prompt == 0 {
				return "0%"
			}
			return formatFloat(float64(cached)/float64(prompt)*100, 1) + "%"
		},
	}
	t := template.Must(template.New("").Funcs(funcs).ParseFS(assets, "web/templates/*.html"))
	return t
}()

// templatesFS returns the templates subdirectory as an fs.FS (for debugging).
func templatesFS() fs.FS {
	sub, err := fs.Sub(assets, "web/templates")
	if err != nil {
		panic(err)
	}
	return sub
}

// RenderPage writes a single page wrapped in the layout.
func RenderPage(w io.Writer, name string, data any) error {
	return Templates.ExecuteTemplate(w, "layout.html", map[string]any{
		"Page":   name,
		"Data":   data,
		"Active": name,
	})
}

// ─────────────────────────── formatting helpers ───────────────────────────

func formatIntGrouped(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	s := []byte{}
	for v > 0 {
		s = append([]byte{byte('0' + v%10)}, s...)
		v /= 10
	}
	out := make([]byte, 0, len(s)+len(s)/3)
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func formatFloat(v float64, decimals int) string {
	neg := v < 0
	if neg {
		v = -v
	}
	pow := 1.0
	for i := 0; i < decimals; i++ {
		pow *= 10
	}
	rounded := uint64(v*pow + 0.5)
	intPart := rounded / uint64(pow)
	fracPart := rounded % uint64(pow)
	intS := []byte{}
	if intPart == 0 {
		intS = []byte("0")
	} else {
		for intPart > 0 {
			intS = append([]byte{byte('0' + intPart%10)}, intS...)
			intPart /= 10
		}
	}
	if decimals == 0 {
		if neg {
			return "-" + string(intS)
		}
		return string(intS)
	}
	fracS := make([]byte, decimals)
	for i := decimals - 1; i >= 0; i-- {
		fracS[i] = byte('0' + fracPart%10)
		fracPart /= 10
	}
	if neg {
		return "-" + string(intS) + "." + string(fracS)
	}
	return string(intS) + "." + string(fracS)
}