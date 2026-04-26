package bot

import "github.com/nextlevelbuilder/goclaw/internal/channels/zalo/common"

// StripMarkdown is preserved as a thin re-export so external callers
// (e.g. zalo/personal) keep working after the markdown helper moved to
// the shared common/ package.
func StripMarkdown(text string) string { return common.StripMarkdown(text) }
