package ui

// The no-hover-raise rule (spec §8.1, D10): hover may only change fill and
// rim brightness. This lint parses every CSS rule whose selector contains
// :hover and fails on anything that moves, scales or elevates the element,
// and it fails if the JS writes a transform (or similar) from a mouseenter or
// mouseover handler.

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

type cssDecl struct{ prop, value string }

type cssRule struct {
	selector string
	decls    []cssDecl
}

// parseCSS returns every style rule, with nested selectors combined and
// grouping at-rules (@media, @supports, ...) flattened.
func parseCSS(src string) []cssRule {
	var out []cssRule
	parseCSSBlock(stripCSSComments(src), "", &out)
	return out
}

func stripCSSComments(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "/*")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		j := strings.Index(s[i+2:], "*/")
		if j < 0 {
			return b.String()
		}
		b.WriteByte(' ')
		s = s[i+2+j+2:]
	}
}

// skipString returns the index just past the string literal starting at i.
func skipString(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case q:
			return j + 1
		}
	}
	return len(s)
}

// matchClose returns the index of the bracket closing the one at s[open].
func matchClose(s string, open int) int {
	o := s[open]
	c := map[byte]byte{'{': '}', '(': ')', '[': ']'}[o]
	depth := 0
	for i := open; i < len(s); {
		switch s[i] {
		case '"', '\'', '`':
			i = skipString(s, i)
			continue
		case o:
			depth++
		case c:
			depth--
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return len(s)
}

var groupingAtRules = map[string]bool{
	"media": true, "supports": true, "layer": true, "container": true,
	"scope": true, "document": true, "starting-style": true,
}

func parseCSSBlock(s, parent string, out *[]cssRule) []cssDecl {
	var decls []cssDecl
	for i := 0; i < len(s); {
		j := i
		for j < len(s) && s[j] != '{' && s[j] != ';' && s[j] != '}' {
			if s[j] == '"' || s[j] == '\'' {
				j = skipString(s, j)
				continue
			}
			j++
		}
		prelude := strings.TrimSpace(s[i:min(j, len(s))])
		if j >= len(s) || s[j] != '{' {
			if prelude != "" && !strings.HasPrefix(prelude, "@") {
				decls = append(decls, parseDecl(prelude))
			}
			i = j + 1
			continue
		}
		end := matchClose(s, j)
		inner := s[j+1 : min(end, len(s))]
		if strings.HasPrefix(prelude, "@") {
			name := strings.ToLower(strings.TrimPrefix(strings.Fields(prelude)[0], "@"))
			if groupingAtRules[name] {
				if nested := parseCSSBlock(inner, parent, out); parent != "" && len(nested) > 0 {
					*out = append(*out, cssRule{selector: parent, decls: nested})
				}
			}
			// @keyframes, @font-face and similar hold no hover selectors.
		} else {
			sel := prelude
			if parent != "" {
				sel = parent + " " + prelude
			}
			d := parseCSSBlock(inner, sel, out)
			*out = append(*out, cssRule{selector: sel, decls: d})
		}
		i = end + 1
	}
	return decls
}

func parseDecl(s string) cssDecl {
	prop, value, _ := strings.Cut(s, ":")
	value = strings.TrimSpace(strings.Replace(value, "!important", "", 1))
	return cssDecl{prop: strings.ToLower(strings.TrimSpace(prop)), value: strings.ToLower(value)}
}

// Properties a :hover rule may never set: they move, scale or elevate.
var raiseProps = map[string]bool{
	"transform": true, "-webkit-transform": true, "translate": true, "scale": true, "zoom": true,
	"box-shadow": true, "-webkit-box-shadow": true, "top": true, "margin-top": true,
	"inset": true, "inset-block-start": true, "margin-block-start": true,
	"animation": true, "animation-name": true,
}

var raiseFuncs = regexp.MustCompile(`\b(translate(x|y|z|3d)?|scale(x|y|z|3d)?|matrix(3d)?|perspective)\s*\(`)
var cssVarRef = regexp.MustCompile(`var\(\s*(--[a-z0-9_-]+)`)

// hoverCSSViolations lists every way the stylesheet raises an element on hover.
func hoverCSSViolations(src string) []string {
	rules := parseCSS(src)
	var bad []string
	hoverVars := map[string]string{}
	for _, r := range rules {
		if !strings.Contains(strings.ToLower(r.selector), ":hover") {
			continue
		}
		for _, d := range r.decls {
			switch {
			case raiseProps[d.prop]:
				bad = append(bad, r.selector+" { "+d.prop+": "+d.value+" }")
			case (d.prop == "filter" || d.prop == "-webkit-filter") && strings.Contains(d.value, "drop-shadow"):
				bad = append(bad, r.selector+" { "+d.prop+": "+d.value+" }")
			case raiseFuncs.MatchString(d.value):
				bad = append(bad, r.selector+" { "+d.prop+": "+d.value+" }")
			case strings.HasPrefix(d.prop, "--"):
				hoverVars[d.prop] = r.selector
			}
		}
	}
	// A custom property changed on hover must not feed a raising property.
	for _, r := range rules {
		for _, d := range r.decls {
			if !raiseProps[d.prop] && !raiseFuncs.MatchString(d.value) && !strings.Contains(d.value, "drop-shadow") {
				continue
			}
			for _, m := range cssVarRef.FindAllStringSubmatch(d.value, -1) {
				if sel, ok := hoverVars[m[1]]; ok {
					bad = append(bad, sel+" sets "+m[1]+", used by "+r.selector+" { "+d.prop+": "+d.value+" }")
				}
			}
		}
	}
	return bad
}

var (
	jsHoverListener = regexp.MustCompile("addEventListener\\(\\s*['\"`](mouseenter|mouseover|pointerenter|pointerover)['\"`]\\s*,")
	jsHoverProperty = regexp.MustCompile(`\.on(mouseenter|mouseover|pointerenter|pointerover)\s*=`)
	jsRaiseWrite    = regexp.MustCompile(`style\s*\.\s*(transform|webkitTransform|translate|scale|zoom|boxShadow|top|marginTop)\s*=[^=]|` +
		`style\s*\[\s*['"](transform|translate|scale|boxShadow|top|marginTop)['"]\s*\]\s*=[^=]|` +
		`setProperty\(\s*['"](transform|translate|scale|box-shadow|top|margin-top)['"]|` +
		`\.animate\(`)
	jsIdent = regexp.MustCompile(`^[A-Za-z_$][\w$]*(\.[A-Za-z_$][\w$]*)*$`)
)

// hoverJSViolations reports mouseenter/mouseover handlers that write a
// transform (or another raising style), inline or through a named function.
func hoverJSViolations(src string) []string {
	var bad []string
	check := func(where, body string) {
		if m := jsRaiseWrite.FindString(body); m != "" {
			bad = append(bad, where+": "+strings.TrimSpace(m))
		}
	}
	for _, loc := range jsHoverListener.FindAllStringIndex(src, -1) {
		open := strings.LastIndex(src[:loc[1]], "addEventListener(") + len("addEventListener")
		call := src[open+1 : min(matchClose(src, open), len(src))]
		_, handler, _ := strings.Cut(call, ",")
		handler = strings.TrimSpace(handler)
		if h, _, ok := strings.Cut(handler, ","); ok && jsIdent.MatchString(strings.TrimSpace(h)) {
			handler = strings.TrimSpace(h) // listener options follow a named handler
		}
		if jsIdent.MatchString(handler) {
			handler = functionBody(src, handler)
		}
		check("hover listener", handler)
	}
	for _, loc := range jsHoverProperty.FindAllStringIndex(src, -1) {
		rest := strings.TrimSpace(src[loc[1]:])
		if jsIdent.MatchString(firstToken(rest)) && !strings.HasPrefix(rest, "function") {
			check("hover property", functionBody(src, firstToken(rest)))
			continue
		}
		check("hover property", statementOrBlock(rest))
	}
	return bad
}

func firstToken(s string) string {
	end := strings.IndexFunc(s, func(r rune) bool {
		return !(r == '_' || r == '$' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	})
	if end < 0 {
		return s
	}
	return s[:end]
}

// statementOrBlock returns the function body starting in s: the first {...}
// block, or for a brace-less arrow function the expression up to ';' or a newline.
func statementOrBlock(s string) string {
	brace := strings.IndexByte(s, '{')
	stop := strings.IndexAny(s, ";\n")
	if brace >= 0 && (stop < 0 || brace < stop) {
		return s[brace:min(matchClose(s, brace)+1, len(s))]
	}
	if stop < 0 {
		return s
	}
	return s[:stop]
}

// functionBody resolves a named handler (function declaration, or a variable
// or property assigned a function) to its body.
func functionBody(src, name string) string {
	last := name[strings.LastIndexByte(name, '.')+1:]
	q := regexp.QuoteMeta(last)
	decl := regexp.MustCompile(`function\s+` + q + `\s*\(|\b` + q + `\s*[:=]\s*(async\s+)?(function\b|\(|[A-Za-z_$][\w$]*\s*=>)|` +
		`\b` + q + `\s*\([^()]*\)\s*\{`) // method shorthand
	var bodies []string
	for _, loc := range decl.FindAllStringIndex(src, -1) {
		bodies = append(bodies, statementOrBlock(src[loc[0]:]))
	}
	return strings.Join(bodies, "\n")
}

func webFiles(t *testing.T, ext string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ext) {
			data, err := fs.ReadFile(webFS, p)
			if err != nil {
				return err
			}
			out[p] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("no %s files embedded", ext)
	}
	return out
}

func TestNoHoverRaise(t *testing.T) {
	hoverRules := 0
	for name, src := range webFiles(t, ".css") {
		for _, r := range parseCSS(src) {
			if strings.Contains(r.selector, ":hover") {
				hoverRules++
			}
		}
		for _, v := range hoverCSSViolations(src) {
			t.Errorf("%s: hover raise: %s", name, v)
		}
	}
	if hoverRules == 0 {
		t.Fatal("parsed no :hover rules at all; the CSS parser is broken")
	}
	for name, src := range webFiles(t, ".js") {
		for _, v := range hoverJSViolations(src) {
			t.Errorf("%s: hover raise: %s", name, v)
		}
	}
	for name, src := range webFiles(t, ".html") {
		if regexp.MustCompile(`(?i)\son(mouseenter|mouseover|pointerenter|pointerover)\s*=`).MatchString(src) {
			t.Errorf("%s: inline hover handler", name)
		}
	}
}

// TestHoverLintCatchesRaise proves the lint fails on the patterns it exists to stop.
func TestHoverLintCatchesRaise(t *testing.T) {
	st, err := loadStatic()
	if err != nil {
		t.Fatal(err)
	}
	appCSS := string(st["/app.css"].data)

	badCSS := []string{
		appCSS + "\n:hover{transform:scale(1.02)}",
		appCSS + "\n.btn:hover { transform: translateY(-1px); }",
		`.b:hover{transform:scale(1.02)}`,
		`.card:hover { background: #111; box-shadow: 0 8px 24px rgba(0,0,0,.4) }`,
		`.x:hover{top:-2px}`,
		`.x:hover{margin-top:-2px}`,
		`.x:hover { translate: 0 -2px }`,
		`.x:hover { scale: 1.03 }`,
		`.x:hover{filter:drop-shadow(0 4px 6px #000)}`,
		`.x:hover{filter: brightness(1.1) drop-shadow(0 0 2px red)}`,
		`.x:hover{rotate: 0deg; offset: path('M0 0') translate(0, -3px)}`,
		`@media (hover: hover) { .x:hover { transform: none; } }`,
		`@supports (display:grid) { @media screen { .x:hover { box-shadow: none } } }`,
		`.x { color: red; &:hover { transform: scale(1.05) } }`,
		`.a, .b:hover { transform: translate3d(0,-1px,0) }`,
		`.x:hover { animation: lift 0.2s }`,
		`.x:hover { --lift: -3px } .x { transform: translateY(var(--lift)) }`,
		`.x:HOVER { Transform: Scale(1.02) !important }`,
		`/* a } comment { */ .x:hover { transform: scale(1.02) }`,
		`.x[data-a="}"]:hover { transform: scale(1.02) }`,
	}
	for _, src := range badCSS {
		if len(hoverCSSViolations(src)) == 0 {
			t.Errorf("lint missed a hover raise in:\n%s", tail(src))
		}
	}

	goodCSS := []string{
		appCSS,
		`.b:hover { background-color: rgba(255,255,255,.16); border-color: rgba(255,255,255,.45); color: #fff }`,
		`.b:active { transform: scale(0.95) }`,
		`.b.pressed { transform: scale(0.95) } .b { transition: transform 90ms, background-color 140ms }`,
		`.b:hover { transition: background-color 140ms }`,
		`.b:focus-visible { outline: 2px solid #60A6FF } .b:hover::after { background-color: #fff }`,
		`.x { --lift: -3px; transform: translateY(var(--lift)) } .x:hover { --tint: #fff }`,
		`@keyframes spin { to { transform: rotate(360deg) } } .x:hover { color: red }`,
	}
	for _, src := range goodCSS {
		if v := hoverCSSViolations(src); len(v) != 0 {
			t.Errorf("false positive %v in:\n%s", v, tail(src))
		}
	}

	badJS := []string{
		`btn.addEventListener('mouseenter', function () { btn.style.transform = 'scale(1.02)'; });`,
		`btn.addEventListener("mouseover", (e) => { e.target.style.transform = "translateY(-2px)" })`,
		"btn.addEventListener(`mouseenter`, e => e.currentTarget.style.boxShadow = '0 4px 8px #000')",
		`function lift(e) { e.target.style.transform = 'scale(1.05)'; }
		 card.addEventListener('mouseover', lift, { passive: true });`,
		`const raise = (e) => { e.target.style['transform'] = 'scale(1.05)' };
		 card.addEventListener('mouseenter', raise);`,
		`el.onmouseover = function () { this.style.top = '-2px'; };`,
		`el.onmouseenter = () => el.style.setProperty('transform', 'scale(1.02)');`,
		`const h = { enter(e) { e.target.style.marginTop = '-1px' } };
		 x.addEventListener('mouseenter', h.enter);`,
		`x.addEventListener('pointerenter', () => { x.animate([{ transform: 'scale(1)' }, { transform: 'scale(1.02)' }], 140) });`,
	}
	for _, src := range badJS {
		if len(hoverJSViolations(src)) == 0 {
			t.Errorf("lint missed a JS hover raise in:\n%s", src)
		}
	}

	goodJS := []string{
		string(st["/app.js"].data),
		string(st["/anim.js"].data),
		`btn.addEventListener('pointerdown', () => { btn.style.transform = 'scale(0.95)'; });`,
		`btn.addEventListener('mouseenter', () => { btn.classList.add('lit'); tip.textContent = 'x'; });`,
		`el.onmouseover = null; el.style.transform = 'scale(1)';`,
		`btn.addEventListener('mouseover', () => { if (btn.style.transform === 'none') log(); });`,
	}
	for _, src := range goodJS {
		if v := hoverJSViolations(src); len(v) != 0 {
			t.Errorf("false positive %v in:\n%s", v, tail(src))
		}
	}
}

// TestIndexHasNoInlineCode keeps index.html compatible with the CSP
// (style-src 'self'; script-src 'self'): no inline styles, scripts or handlers.
func TestIndexHasNoInlineCode(t *testing.T) {
	for name, src := range webFiles(t, ".html") {
		checks := map[string]*regexp.Regexp{
			"<style> element":   regexp.MustCompile(`(?i)<style[\s>]`),
			"style attribute":   regexp.MustCompile(`(?i)\sstyle\s*=`),
			"inline script":     regexp.MustCompile(`(?i)<script(\s[^>]*)?>\s*[^<\s]`),
			"event handler":     regexp.MustCompile(`(?i)\son[a-z]+\s*=`),
			"javascript: URL":   regexp.MustCompile(`(?i)javascript:`),
			"external resource": regexp.MustCompile(`(?i)(src|href)\s*=\s*"(https?:)?//`),
		}
		for what, re := range checks {
			if loc := re.FindStringIndex(src); loc != nil {
				t.Errorf("%s: %s at %q", name, what, src[loc[0]:min(loc[1]+30, len(src))])
			}
		}
	}
}

func tail(s string) string {
	if len(s) > 200 {
		return "..." + s[len(s)-200:]
	}
	return s
}
