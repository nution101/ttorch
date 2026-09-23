package board

import (
	"fmt"
	"html/template"
	"strings"
	"time"
)

// templateFuncs are the page template's helpers. Each returns plain text, which
// html/template then escapes like any other value.
var templateFuncs = template.FuncMap{
	"ago":  ago,
	"join": func(xs []string) string { return strings.Join(xs, ", ") },
}

// ago renders a time as a short relative age. It accepts a time.Time or a *time.Time so the
// template can pass optional fields straight through.
func ago(v any) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x == nil {
			return ""
		}
		t = *x
	default:
		return ""
	}
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Local().Format("Jan 2 15:04")
	}
}
