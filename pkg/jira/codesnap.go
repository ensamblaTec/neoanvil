// Package jira — codesnap.go
// PILAR XXIII / Épica 127.B — auto-generate code-snap PNGs from local
// snippets, ready to ship inside the jira-docs/<KEY>/images/ folder
// that attach_artifact zips and uploads.
//
// Pipeline:
//   1. Read source file
//   2. Render to HTML with chroma syntax highlighting
//   3. Wrap in a styled card (header, gradient bg, monospace body)
//   4. Drive headless Chrome via chromedp to screenshot the card
//   5. Write PNG bytes to disk
//
// chromedp finds Chrome by path:
//   - $CHROMEDP_CHROME or $CHROME_BIN env var (override)
//   - /Applications/Google Chrome.app/Contents/MacOS/Google Chrome
//   - chromium-browser, chromium, google-chrome on Linux $PATH
// If none available, RenderCodeSnap returns an error and the caller
// can fall back to the HTML file (still useful for browser viewing).

package jira

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/chroma/v2"
	chromaformatters "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/chromedp/chromedp"
)

// CodeSnapInput drives one snippet → PNG render.
type CodeSnapInput struct {
	SourcePath string // required: file to read
	Title      string // optional header (defaults to filename)
	OutPNG     string // required: where to write the PNG
	Theme      string // chroma style name; default "github-dark"
	Language   string // override lexer; auto-detect from extension when empty
	Width      int    // viewport width in px (default 980)
}

// CodeSnapResult reports what was rendered.
type CodeSnapResult struct {
	HTMLPath string // intermediate HTML kept alongside the PNG
	PNGPath  string
	Lines    int
	Bytes    int64
}

// RenderCodeSnap generates an HTML+PNG pair for a single source file.
// Returns the result + paths. The PNG is suitable to drop into the
// jira-docs/<KEY>/images/ folder before attach_artifact zips it.
//
// If headless Chrome is unavailable on the host, returns an error after
// writing the HTML so the operator still has the syntax-highlighted
// markup (visually viewable in any browser, no PNG).
func RenderCodeSnap(ctx context.Context, in CodeSnapInput) (*CodeSnapResult, error) {
	if strings.TrimSpace(in.SourcePath) == "" || strings.TrimSpace(in.OutPNG) == "" {
		return nil, errors.New("SourcePath and OutPNG are required")
	}
	src, err := os.ReadFile(in.SourcePath) //nolint:gosec // G304-CLI-CONSENT: operator-supplied snippet path under jira-docs scratch
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", in.SourcePath, err)
	}

	htmlBody, lexerName := renderHighlightedHTML(src, in.SourcePath, in.Language, in.Theme)
	title := in.Title
	if title == "" {
		title = filepath.Base(in.SourcePath)
	}
	docHTML := buildSnapHTML(title, lexerName, htmlBody, in.Width)

	htmlPath := strings.TrimSuffix(in.OutPNG, filepath.Ext(in.OutPNG)) + ".html"
	if err := os.WriteFile(htmlPath, []byte(docHTML), 0o600); err != nil {
		return nil, fmt.Errorf("write html: %w", err)
	}
	res := &CodeSnapResult{
		HTMLPath: htmlPath,
		PNGPath:  in.OutPNG,
		Lines:    bytes.Count(src, []byte{'\n'}) + 1,
		Bytes:    int64(len(src)),
	}

	if err := htmlToPNG(ctx, htmlPath, in.OutPNG, in.Width); err != nil {
		return res, fmt.Errorf("html→png (HTML kept at %s): %w", htmlPath, err)
	}
	return res, nil
}

// renderHighlightedHTML applies chroma highlighting and returns the
// inner body HTML. Lexer is detected from filename when languageOverride
// is empty. Falls back to plaintext lexer when nothing matches.
func renderHighlightedHTML(src []byte, srcPath, languageOverride, theme string) (string, string) {
	lexer := pickLexer(srcPath, languageOverride, src)
	style := styles.Get(theme)
	if style == nil {
		style = styles.Get("github-dark")
	}
	if style == nil {
		style = styles.Fallback
	}
	formatter := chromaformatters.New(
		chromaformatters.WithClasses(false), // inline styles for self-contained HTML
		chromaformatters.WithLineNumbers(true),
		chromaformatters.LineNumbersInTable(true),
		chromaformatters.TabWidth(4),
	)
	iterator, err := lexer.Tokenise(nil, string(src))
	if err != nil {
		return "<pre>" + html.EscapeString(string(src)) + "</pre>", "plaintext"
	}
	buf := &bytes.Buffer{}
	if err := formatter.Format(buf, style, iterator); err != nil {
		return "<pre>" + html.EscapeString(string(src)) + "</pre>", lexer.Config().Name
	}
	return buf.String(), lexer.Config().Name
}

// pickLexer chooses a chroma lexer by override, then by filename, then
// by content sniffing. Defaults to plaintext.
func pickLexer(srcPath, override string, content []byte) chroma.Lexer {
	if override != "" {
		if lex := lexers.Get(override); lex != nil {
			return lex
		}
	}
	if lex := lexers.Match(filepath.Base(srcPath)); lex != nil {
		return lex
	}
	if lex := lexers.Analyse(string(content)); lex != nil {
		return lex
	}
	return lexers.Fallback
}

// buildSnapHTML wraps the chroma output in a card-style document with
// header, gradient background, and constrained width — looks closer to
// a "code-snap" image than raw chroma output.
func buildSnapHTML(title, lexerName, body string, width int) string {
	if width <= 0 {
		width = 980
	}
	tmpl := `<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"/>
<title>%s</title>
<style>
  html,body { margin:0; padding:0; background:#1e1e2e; }
  body { padding:32px; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; }
  .card {
    width: %dpx;
    background: linear-gradient(135deg, #1f2937 0%%, #111827 100%%);
    border-radius: 12px;
    overflow: hidden;
    box-shadow: 0 16px 36px rgba(0,0,0,0.45);
    border: 1px solid rgba(148,163,184,0.18);
  }
  .header {
    display: flex; align-items: center; gap: 12px;
    padding: 14px 20px;
    background: rgba(15,23,42,0.6);
    border-bottom: 1px solid rgba(148,163,184,0.15);
    color: #cbd5e1;
    font-size: 13px;
  }
  .dot { width: 12px; height: 12px; border-radius: 50%%; display: inline-block; }
  .dot.red { background: #ef4444; }
  .dot.yellow { background: #f59e0b; }
  .dot.green { background: #10b981; }
  .filename { font-weight: 600; color: #e2e8f0; }
  .lang { margin-left: auto; padding: 2px 8px; border-radius: 999px; background: rgba(99,102,241,0.18); color: #c4b5fd; font-size: 11px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
  .body { padding: 0; font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace; font-size: 13px; }
  .body pre { margin:0; padding: 18px 22px; line-height: 1.5; overflow-x: auto; }
  .body table { border-collapse: collapse; width: 100%%; }
  .body td { padding: 0; vertical-align: top; }
  .body td:first-child { user-select: none; opacity: 0.45; padding-right: 14px; text-align: right; }
</style></head>
<body>
  <div class="card">
    <div class="header">
      <span class="dot red"></span><span class="dot yellow"></span><span class="dot green"></span>
      <span class="filename">%s</span>
      <span class="lang">%s</span>
    </div>
    <div class="body">%s</div>
  </div>
</body></html>`
	return fmt.Sprintf(tmpl,
		html.EscapeString(title),
		width,
		html.EscapeString(title),
		html.EscapeString(lexerName),
		body,
	)
}

// htmlToPNG drives headless Chrome via chromedp to screenshot the
// rendered card. Loads the HTML via file:// URL, waits for layout, and
// captures the .card element with its full bounding box.
func htmlToPNG(ctx context.Context, htmlPath, pngPath string, width int) error {
	chromePath, err := resolveChromePath()
	if err != nil {
		return err
	}
	if width <= 0 {
		width = 980
	}

	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.WindowSize(width+96, 800),
		chromedp.NoSandbox,
		chromedp.DisableGPU,
		chromedp.Flag("headless", "new"),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-extensions", true),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, allocOpts...)
	defer allocCancel()

	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()

	// First-launch of macOS Chrome (with code-signing verification)
	// can take 15-25s on cold cache. 60s gives us 2x headroom.
	timed, timedCancel := context.WithTimeout(browserCtx, 60*time.Second)
	defer timedCancel()

	absHTML, err := filepath.Abs(htmlPath)
	if err != nil {
		return fmt.Errorf("abs html: %w", err)
	}
	url := "file://" + absHTML

	var buf []byte
	if err := chromedp.Run(timed,
		chromedp.Navigate(url),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Sleep(300*time.Millisecond), // let layout settle
		chromedp.Screenshot(".card", &buf, chromedp.NodeVisible, chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("chromedp run: %w", err)
	}
	return os.WriteFile(pngPath, buf, 0o600)
}

// resolveChromePath returns the first Chrome/Chromium binary found on
// the host. Honors $CHROMEDP_CHROME and $CHROME_BIN env overrides for
// CI environments.
func resolveChromePath() (string, error) {
	for _, env := range []string{"CHROMEDP_CHROME", "CHROME_BIN"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			if _, err := os.Stat(v); err == nil {
				return v, nil
			}
		}
	}
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
		"/snap/bin/chromium",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no Chrome/Chromium binary found (set CHROMEDP_CHROME or install Chrome)")
}
