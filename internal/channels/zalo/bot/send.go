package bot

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
)

const maxMediaBytes = 10 * 1024 * 1024 // 10MB

// isHTTPURL reports whether u is an http or https URL. Bot's sendPhoto API
// only accepts remote URLs; local paths must be rejected.
func isHTTPURL(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// mergeTrailingText joins caption + content with a blank line. Mirrors
// zalo/oa's mergeTrailingText so users see consistent layout across channels.
func mergeTrailingText(caption, content string) string {
	caption = strings.TrimSpace(caption)
	content = strings.TrimSpace(content)
	switch {
	case caption == "" && content == "":
		return ""
	case caption == "":
		return content
	case content == "":
		return caption
	default:
		return caption + "\n\n" + content
	}
}

func (c *Channel) sendChunkedText(chatID, text string) error {
	for _, chunk := range channels.ChunkMarkdown(text, maxTextLength) {
		if err := c.sendMessage(chatID, chunk); err != nil {
			return err
		}
	}
	return nil
}

// downloadMedia fetches a photo from a Zalo CDN URL and saves it as a local temp file.
// Zalo CDN URLs are auth-restricted and expire, so we must download immediately.
func (c *Channel) downloadMedia(url string) (string, error) {
	resp, err := c.client.Get(url)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}

	// Detect extension from Content-Type
	ext := ".jpg"
	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.Contains(ct, "png"):
		ext = ".png"
	case strings.Contains(ct, "gif"):
		ext = ".gif"
	case strings.Contains(ct, "webp"):
		ext = ".webp"
	}

	f, err := os.CreateTemp("", "goclaw_zalo_*"+ext)
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	defer f.Close()

	n, err := io.Copy(f, io.LimitReader(resp.Body, maxMediaBytes))
	if err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write: %w", err)
	}
	if n == 0 {
		os.Remove(f.Name())
		return "", fmt.Errorf("empty response")
	}

	slog.Debug("zalo media downloaded", "path", f.Name(), "size", n)
	return f.Name(), nil
}
