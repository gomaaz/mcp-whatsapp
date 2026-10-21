package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Defaults for local transcription with the whisper.cpp CLI.
const (
	DefaultWhisperLang        = "auto"
	DefaultWhisperTimeout     = 5 * time.Minute
	DefaultWhisperMaxDuration = 15 * time.Minute
)

// Environment variables that configure transcription. Only BIN and MODEL are
// required; without them transcription is off.
const (
	EnvWhisperBin         = "WHATSAPP_MCP_WHISPER_BIN"
	EnvWhisperModel       = "WHATSAPP_MCP_WHISPER_MODEL"
	EnvWhisperLang        = "WHATSAPP_MCP_WHISPER_LANG"
	EnvWhisperTimeout     = "WHATSAPP_MCP_WHISPER_TIMEOUT"
	EnvWhisperMaxDuration = "WHATSAPP_MCP_WHISPER_MAX_DURATION"
)

// wavBytesPerSecond is the data rate of the 16 kHz mono 16-bit PCM WAV that
// whisper.cpp expects.
const wavBytesPerSecond = 16000 * 2

// ErrWhisperNotConfigured is returned when transcription is requested but the
// whisper.cpp binary or model is not set.
var ErrWhisperNotConfigured = errors.New("voice-note transcription is not configured: set " +
	EnvWhisperBin + " to the whisper.cpp CLI (e.g. C:\\whisper\\whisper-cli.exe) and " +
	EnvWhisperModel + " to a ggml model file (e.g. ggml-base.bin), then restart the daemon; optional: " +
	EnvWhisperLang + " (default auto), " + EnvWhisperTimeout + " (default 5m), " +
	EnvWhisperMaxDuration + " (default 15m)")

// ErrAudioTooLong is returned when an audio message exceeds the configured
// maximum duration.
var ErrAudioTooLong = errors.New("audio is longer than the transcription limit")

// ErrNoSpeech is returned when whisper.cpp ran but produced no text.
var ErrNoSpeech = errors.New("whisper produced no text (silent or unrecognised audio)")

// WhisperConfig configures local transcription with the whisper.cpp CLI.
type WhisperConfig struct {
	// Bin is the whisper.cpp CLI (current releases name it whisper-cli).
	Bin string
	// Model is the ggml model file passed with -m.
	Model string
	// Lang is the spoken language passed with -l ("auto" to detect it).
	Lang string
	// Timeout bounds the whole conversion and transcription.
	Timeout time.Duration
	// MaxDuration is the longest audio that is transcribed.
	MaxDuration time.Duration
	// FFmpeg is the ffmpeg executable; empty means "ffmpeg" from PATH.
	FFmpeg string
}

// langRe accepts "auto" or an ISO 639-1/639-2 language code, the forms
// whisper.cpp understands.
var langRe = regexp.MustCompile(`^(auto|[a-z]{2,3})$`)

// WhisperConfigFromEnv reads the transcription settings through getenv
// (os.Getenv in production). Missing BIN or MODEL is not an error: the
// result is simply not Enabled. A malformed language, timeout or maximum
// duration is.
func WhisperConfigFromEnv(getenv func(string) string) (WhisperConfig, error) {
	cfg := WhisperConfig{
		Bin:   strings.TrimSpace(getenv(EnvWhisperBin)),
		Model: strings.TrimSpace(getenv(EnvWhisperModel)),
		Lang:  strings.ToLower(strings.TrimSpace(getenv(EnvWhisperLang))),
	}
	if cfg.Lang == "" {
		cfg.Lang = DefaultWhisperLang
	}
	if !langRe.MatchString(cfg.Lang) {
		return WhisperConfig{}, fmt.Errorf("%s=%q: want auto or a language code such as de or en", EnvWhisperLang, cfg.Lang)
	}
	var err error
	if cfg.Timeout, err = parseDurationEnv(EnvWhisperTimeout, getenv(EnvWhisperTimeout), DefaultWhisperTimeout); err != nil {
		return WhisperConfig{}, err
	}
	if cfg.MaxDuration, err = parseDurationEnv(EnvWhisperMaxDuration, getenv(EnvWhisperMaxDuration), DefaultWhisperMaxDuration); err != nil {
		return WhisperConfig{}, err
	}
	return cfg, nil
}

// parseDurationEnv parses a Go duration ("90s", "5m") or a plain number of
// seconds. Empty means def; zero or negative values are rejected.
func parseDurationEnv(name, raw string, def time.Duration) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		secs, convErr := strconv.Atoi(raw)
		if convErr != nil {
			return 0, fmt.Errorf("%s=%q: want a duration such as 5m or a number of seconds", name, raw)
		}
		d = time.Duration(secs) * time.Second
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s=%q: must be positive", name, raw)
	}
	return d, nil
}

// Enabled reports whether a whisper.cpp binary and model are configured.
func (c WhisperConfig) Enabled() bool {
	return c.Bin != "" && c.Model != ""
}

// Check verifies that transcription is configured and that the binary and
// model exist. Error messages name the environment variable to fix and only
// the base name of the configured path, not the full local layout.
func (c WhisperConfig) Check() error {
	if !c.Enabled() {
		return ErrWhisperNotConfigured
	}
	if _, err := exec.LookPath(c.Bin); err != nil {
		return fmt.Errorf("%s: %s is not an executable file", EnvWhisperBin, filepath.Base(c.Bin))
	}
	info, err := os.Stat(c.Model)
	if err != nil || info.IsDir() {
		return fmt.Errorf("%s: model file %s not found", EnvWhisperModel, filepath.Base(c.Model))
	}
	return nil
}

func (c WhisperConfig) lang() string {
	if c.Lang == "" {
		return DefaultWhisperLang
	}
	return c.Lang
}

func (c WhisperConfig) timeout() time.Duration {
	if c.Timeout <= 0 {
		return DefaultWhisperTimeout
	}
	return c.Timeout
}

func (c WhisperConfig) maxDuration() time.Duration {
	if c.MaxDuration <= 0 {
		return DefaultWhisperMaxDuration
	}
	return c.MaxDuration
}

// CheckDuration rejects audio whose known length exceeds the limit. Zero
// seconds means unknown and passes; Transcribe measures the converted audio
// again either way.
func (c WhisperConfig) CheckDuration(seconds uint32) error {
	if limit := c.maxDuration(); seconds > 0 && time.Duration(seconds)*time.Second > limit {
		return fmt.Errorf("%w: %d s > %s (%s)", ErrAudioTooLong, seconds, limit, EnvWhisperMaxDuration)
	}
	return nil
}

// Transcribe converts the audio file at inputPath to 16 kHz mono WAV with
// ffmpeg and runs the whisper.cpp CLI on it, returning the recognised text on
// one line. Both tools run in a private temp directory (removed afterwards)
// with a minimal environment, under cfg.Timeout. Audio longer than
// cfg.MaxDuration is rejected after conversion, whatever its metadata says.
func Transcribe(ctx context.Context, cfg WhisperConfig, inputPath string) (string, error) {
	if err := cfg.Check(); err != nil {
		return "", err
	}
	ffmpeg := cfg.FFmpeg
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	if _, err := exec.LookPath(ffmpeg); err != nil {
		return "", errors.New("ffmpeg not found on PATH; install ffmpeg to transcribe voice notes")
	}

	timeout := cfg.timeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "mcp-whatsapp-transcribe-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	wavPath := filepath.Join(dir, "audio.wav")
	if _, stderr, err := runTool(ctx, ffmpeg, ffmpegWAVArgs(inputPath, wavPath, cfg.maxDuration())); err != nil {
		return "", toolError(ctx, "ffmpeg conversion", timeout, err, stderr)
	}
	info, err := os.Stat(wavPath)
	if err != nil {
		return "", fmt.Errorf("ffmpeg produced no audio: %w", err)
	}
	if secs := wavSeconds(info.Size()); secs > cfg.maxDuration().Seconds() {
		return "", fmt.Errorf("%w: over %s (%s)", ErrAudioTooLong, cfg.maxDuration(), EnvWhisperMaxDuration)
	}

	outBase := filepath.Join(dir, "transcript")
	stdout, stderr, err := runTool(ctx, cfg.Bin, whisperArgs(cfg, wavPath, outBase))
	if err != nil {
		return "", toolError(ctx, "whisper", timeout, err, stderr)
	}
	text := readTranscript(outBase+".txt", stdout)
	if text == "" {
		return "", ErrNoSpeech
	}
	return text, nil
}

// ffmpegWAVArgs builds the ffmpeg arguments that turn any audio into the
// 16 kHz mono 16-bit WAV whisper.cpp reads. Output is cut one second past the
// limit, so an overlong file is detected without converting all of it.
func ffmpegWAVArgs(inputPath, wavPath string, maxDuration time.Duration) []string {
	limit := strconv.FormatFloat(maxDuration.Seconds()+1, 'f', -1, 64)
	return []string{
		"-nostdin",
		"-hide_banner",
		"-loglevel", "error",
		"-i", sanitizeFFmpegInputPath(inputPath),
		"-t", limit,
		"-vn",
		"-ar", "16000",
		"-ac", "1",
		"-c:a", "pcm_s16le",
		"-y",
		wavPath,
	}
}

// whisperArgs builds the whisper.cpp CLI arguments: model, input, language,
// no timestamps, no log output on stdout, and the text also written to
// <outBase>.txt.
func whisperArgs(cfg WhisperConfig, wavPath, outBase string) []string {
	return []string{
		"-m", cfg.Model,
		"-f", wavPath,
		"-l", cfg.lang(),
		"-nt",
		"-np",
		"-otxt",
		"-of", outBase,
	}
}

// wavSeconds estimates the length of a 16 kHz mono 16-bit WAV from its file
// size; the header (44 bytes, a little more with metadata) is negligible.
func wavSeconds(size int64) float64 {
	if size <= 44 {
		return 0
	}
	return float64(size-44) / wavBytesPerSecond
}

// readTranscript prefers the .txt file whisper.cpp wrote with -otxt and falls
// back to its stdout. The text is collapsed onto one line and cleaned by
// speechText, so audio without speech comes back as "".
func readTranscript(txtPath string, stdout []byte) string {
	if data, err := os.ReadFile(txtPath); err == nil {
		if text := speechText(string(data)); text != "" {
			return text
		}
	}
	return speechText(string(stdout))
}

// blankAudioRe matches whisper.cpp's marker for a stretch without sound.
var blankAudioRe = regexp.MustCompile(`(?i)\[\s*blank_audio\s*\]`)

// annotationsOnlyRe matches text made only of bracketed, parenthesised or
// starred annotations, which is how whisper labels audio without speech:
// "[ Silence ]", "(music)", "[Musik]", "*Applaus*".
var annotationsOnlyRe = regexp.MustCompile(`^(?:(?:\[[^\]]*\]|\([^)]*\)|\*[^*]*\*)\s*)+$`)

// speechText collapses whisper output onto one line and returns "" when it
// holds no speech: [BLANK_AUDIO] markers are removed wherever they are, and
// what is left counts as no speech when it consists only of annotations.
// Annotations next to spoken words stay, they may be part of the text.
func speechText(raw string) string {
	text := strings.Join(strings.Fields(blankAudioRe.ReplaceAllString(raw, " ")), " ")
	if annotationsOnlyRe.MatchString(text) {
		return ""
	}
	return text
}

// windowsEnvKeys are passed in addition to minimalEnv: Windows programs need
// them to load system DLLs. Unset on other systems.
var windowsEnvKeys = []string{"SYSTEMROOT", "WINDIR"}

// runTool runs name with args under ctx in a minimal environment and returns
// its stdout and stderr.
func runTool(ctx context.Context, name string, args []string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	env := minimalEnv()
	for _, k := range windowsEnvKeys {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env = env
	// A child that leaves a grandchild holding the pipes must not keep the
	// call alive past the timeout.
	cmd.WaitDelay = 5 * time.Second
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = cmd.Run()
	return out.Bytes(), errOut.Bytes(), err
}

// toolError reports a failed tool run, naming a timeout as such. The tail of
// stderr is kept (the reason is usually at the end), with paths removed.
func toolError(ctx context.Context, what string, timeout time.Duration, err error, stderr []byte) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s timed out after %s (%s)", what, timeout, EnvWhisperTimeout)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s cancelled: %w", what, ctx.Err())
	}
	msg := strings.TrimSpace(string(stderr))
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[len(r)-200:])
	}
	return fmt.Errorf("%s failed: %w: %s", what, err, sanitizeStderr(winPathRe.ReplaceAllString(msg, "<path>")))
}

// winPathRe matches Windows drive paths (C:\dir\file, C:/dir/file), which
// absPathRe does not cover.
var winPathRe = regexp.MustCompile(`[A-Za-z]:[\\/][^\s"'<>|]*`)
