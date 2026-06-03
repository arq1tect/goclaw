package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/providers"
	usagecaps "github.com/nextlevelbuilder/goclaw/internal/usage/caps"
)

// --- Context helpers for media audio ---

const ctxMediaAudioRefs toolContextKey = "tool_media_audio_refs"

// WithMediaAudioRefs stores audio MediaRefs in context for read_audio tool access.
func WithMediaAudioRefs(ctx context.Context, refs []providers.MediaRef) context.Context {
	return context.WithValue(ctx, ctxMediaAudioRefs, refs)
}

// MediaAudioRefsFromCtx retrieves stored audio MediaRefs from context.
func MediaAudioRefsFromCtx(ctx context.Context) []providers.MediaRef {
	v, _ := ctx.Value(ctxMediaAudioRefs).([]providers.MediaRef)
	return v
}

// --- ReadAudioTool ---

// audioMaxBytes is the max file size for audio analysis (50MB).
const audioMaxBytes = 50 * 1024 * 1024

// audioProviderPriority is the order in which providers are tried for audio analysis.
var audioProviderPriority = []string{"gemini", "openai", "openrouter"}

// audioModelDefaults maps provider names to preferred audio-capable models.
var audioModelDefaults = map[string]string{
	"gemini":     "gemini-2.5-flash",
	"openai":     "gpt-4o-audio-preview",
	"openrouter": "google/gemini-2.5-flash",
}

// ReadAudioTool uses an audio-capable provider to analyze audio files
// attached to the current conversation.
type ReadAudioTool struct {
	registry    *providers.Registry
	mediaLoader MediaPathLoader
	usageCaps   *usagecaps.Service
}

func NewReadAudioTool(registry *providers.Registry, mediaLoader MediaPathLoader) *ReadAudioTool {
	return &ReadAudioTool{registry: registry, mediaLoader: mediaLoader}
}

func (t *ReadAudioTool) SetUsageCapService(svc *usagecaps.Service) {
	t.usageCaps = svc
}

func (t *ReadAudioTool) Name() string { return "read_audio" }

func (t *ReadAudioTool) Description() string {
	return "Analyze audio files (speech, music, sounds). Works with: " +
		"(1) <media:audio> / <media:voice> tags from the conversation (use media_id, or omit for most recent), " +
		"(2) audio files on disk (pass a file path — required when running via MCP bridge / Claude CLI provider, " +
		"where the path attribute is included in the media tag)."
}

func (t *ReadAudioTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"description": "What to analyze. E.g. 'Transcribe this audio', 'Summarize the conversation', 'What language is spoken?'",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Optional file path to an audio file. Use the path attribute from the <media:audio> or <media:voice> tag. Takes priority over media_id.",
			},
			"media_id": map[string]any{
				"type":        "string",
				"description": "Optional: specific media_id from <media:audio>/<media:voice> tag. Used only when path is not provided. If both omitted, uses most recent audio.",
			},
		},
		"required": []string{"prompt"},
	}
}

func (t *ReadAudioTool) Execute(ctx context.Context, args map[string]any) *Result {
	prompt, _ := args["prompt"].(string)
	if prompt == "" {
		prompt = "Analyze this audio and describe its contents."
	}
	pathArg, _ := args["path"].(string)
	mediaID, _ := args["media_id"].(string)

	audioPath, audioMime, err := t.resolveAudioFile(ctx, mediaID, pathArg)
	if err != nil {
		return ErrorResult(err.Error())
	}

	// Convert non-mp3/wav formats to mp3 for universal provider compatibility.
	// OpenAI input_audio API accepts only mp3/wav. Gemini File API and Whisper
	// transcription both accept mp3 too, so converting once covers all paths.
	// Telegram voice messages are .ogg/Opus → must be converted.
	ext := strings.ToLower(filepath.Ext(audioPath))
	if ext != ".mp3" && ext != ".wav" {
		converted, convErr := convertAudioToMP3(ctx, audioPath)
		if convErr != nil {
			slog.Warn("read_audio: ffmpeg conversion failed, sending original", "ext", ext, "err", convErr)
		} else {
			slog.Info("read_audio: converted to mp3", "src", audioPath, "dst", converted)
			defer os.Remove(converted)
			audioPath = converted
			audioMime = "audio/mpeg"
		}
	}

	slog.Info("read_audio: resolved file", "path", audioPath, "mime", audioMime, "media_id", mediaID)

	data, err := os.ReadFile(audioPath)
	if err != nil {
		return ErrorResult(fmt.Sprintf("Failed to read audio file: %v", err))
	}
	slog.Info("read_audio: file loaded", "size_bytes", len(data))
	if len(data) > audioMaxBytes {
		return ErrorResult(fmt.Sprintf("Audio too large: %d bytes (max %d)", len(data), audioMaxBytes))
	}

	chain := ResolveMediaProviderChain(ctx, "read_audio", "", "",
		audioProviderPriority, audioModelDefaults, t.registry)

	for i := range chain {
		if chain[i].Params == nil {
			chain[i].Params = make(map[string]any)
		}
		chain[i].Params["prompt"] = prompt
		chain[i].Params["data"] = data
		chain[i].Params["mime"] = audioMime
	}

	chainResult, err := ExecuteWithChain(ctx, chain, t.registry, t.callProvider)
	if err != nil {
		return ErrorResult(fmt.Sprintf("Audio analysis failed: %v", err))
	}

	result := NewResult(string(chainResult.Data))
	result.Usage = chainResult.Usage
	result.Provider = chainResult.Provider
	result.Model = chainResult.Model
	return result
}

// convertAudioToMP3 converts an audio file to mono 16 kHz mp3 via ffmpeg.
// Returns the path to a temp file (caller must remove). Used to normalize
// Telegram voice (.ogg/Opus), .m4a/.aac/.webm, etc. into a format every
// audio-capable provider accepts (OpenAI input_audio, Gemini, Whisper).
func convertAudioToMP3(ctx context.Context, srcPath string) (string, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", fmt.Errorf("ffmpeg not on PATH: %w", err)
	}
	f, err := os.CreateTemp("", "read_audio_*.mp3")
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	dstPath := f.Name()
	f.Close()

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-i", srcPath,
		"-ac", "1", // mono — sufficient for transcription / analysis
		"-ar", "16000", // 16 kHz — matches Whisper's native rate
		"-q:a", "5", // VBR q=5, ~96-128 kbps — good speech quality, small files
		"-loglevel", "error",
		"-y", dstPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(dstPath)
		return "", fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return dstPath, nil
}

// mimeFromAudioExt returns MIME type for audio file extensions.
func mimeFromAudioExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".m4a":
		return "audio/mp4"
	case ".aac":
		return "audio/aac"
	case ".flac":
		return "audio/flac"
	case ".aiff", ".aif":
		return "audio/aiff"
	case ".wma":
		return "audio/x-ms-wma"
	case ".opus":
		return "audio/opus"
	default:
		return "audio/mpeg"
	}
}
