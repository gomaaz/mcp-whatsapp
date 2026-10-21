package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The transcription tests never need real ffmpeg, whisper.cpp or audio: the
// test binary copies itself under the names below, and TestMain hands such a
// copy to runFakeTool. The fakes take their instructions from files, since
// the child only gets a minimal environment.
//
//   - fake-ffmpeg reads "seconds=N" from its -i input (default 2), writes a
//     16 kHz mono WAV of that length to its last argument, and records its
//     arguments in <input>.args.
//   - fake-whisper reads key=value lines from its -m model file: sleep=<dur>,
//     stdout=<text>, txt=<text> (written to <-of>.txt), stderr=<text>,
//     exit=<code>. It records its arguments in <model>.args.
const (
	fakeFFmpegName  = "fake-ffmpeg"
	fakeWhisperName = "fake-whisper"
)

func runFakeTool(args []string) (int, bool) {
	name := strings.TrimSuffix(filepath.Base(args[0]), ".exe")
	switch name {
	case fakeFFmpegName:
		return fakeFFmpeg(args[1:]), true
	case fakeWhisperName:
		return fakeWhisper(args[1:]), true
	}
	return 0, false
}

func argAfter(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func fakeInstructions(path string) map[string]string {
	out := map[string]string{}
	data, _ := os.ReadFile(path)
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			out[k] = strings.ReplaceAll(v, `\n`, "\n")
		}
	}
	return out
}

func fakeFFmpeg(args []string) int {
	in := argAfter(args, "-i")
	_ = os.WriteFile(in+".args", []byte(strings.Join(args, "\n")), 0o600)
	secs := 2
	if v, err := strconv.Atoi(fakeInstructions(in)["seconds"]); err == nil {
		secs = v
	}
	if err := os.WriteFile(args[len(args)-1], make([]byte, 44+secs*wavBytesPerSecond), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func fakeWhisper(args []string) int {
	model := argAfter(args, "-m")
	_ = os.WriteFile(model+".args", []byte(strings.Join(args, "\n")), 0o600)
	in := fakeInstructions(model)
	if d, err := time.ParseDuration(in["sleep"]); err == nil {
		time.Sleep(d)
	}
	if v, ok := in["txt"]; ok {
		_ = os.WriteFile(argAfter(args, "-of")+".txt", []byte(v), 0o600)
	}
	fmt.Fprint(os.Stdout, in["stdout"])
	fmt.Fprint(os.Stderr, in["stderr"])
	code, _ := strconv.Atoi(in["exit"])
	return code
}

// installFake copies the running test binary into dir under name.
func installFake(t *testing.T, dir, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(dir, name)
	src, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return dst
}

// fakeSetup is a configured transcriber whose ffmpeg and whisper are fakes.
type fakeSetup struct {
	cfg   WhisperConfig
	input string
	dir   string
}

// newFakeSetup installs both fakes and writes the model instructions and the
// input audio instructions.
func newFakeSetup(t *testing.T, model string, inputSeconds int) fakeSetup {
	t.Helper()
	dir := t.TempDir()
	cfg := WhisperConfig{
		Bin:         installFake(t, dir, fakeWhisperName),
		FFmpeg:      installFake(t, dir, fakeFFmpegName),
		Model:       filepath.Join(dir, "ggml-test.bin"),
		Lang:        "de",
		Timeout:     30 * time.Second,
		MaxDuration: 10 * time.Second,
	}
	if err := os.WriteFile(cfg.Model, []byte(model), 0o600); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "voice.ogg")
	if err := os.WriteFile(input, []byte(fmt.Sprintf("seconds=%d\n", inputSeconds)), 0o600); err != nil {
		t.Fatal(err)
	}
	return fakeSetup{cfg: cfg, input: input, dir: dir}
}

func readArgs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	return strings.Split(string(data), "\n")
}

func TestTranscribe_ReadsTxtFileAndPassesArgs(t *testing.T) {
	f := newFakeSetup(t, "txt=Hallo Daniel,\\n ich brauche   ein Angebot.\nstdout=ignored\n", 3)
	got, err := Transcribe(context.Background(), f.cfg, f.input)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if want := "Hallo Daniel, ich brauche ein Angebot."; got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}

	whisper := readArgs(t, f.cfg.Model+".args")
	wav, outBase := argAfter(whisper, "-f"), argAfter(whisper, "-of")
	if want := whisperArgs(f.cfg, wav, outBase); !slices.Equal(whisper, want) {
		t.Fatalf("whisper args = %v, want %v", whisper, want)
	}
	if argAfter(whisper, "-l") != "de" || argAfter(whisper, "-m") != f.cfg.Model {
		t.Fatalf("whisper language/model args wrong: %v", whisper)
	}
	if filepath.Dir(wav) != filepath.Dir(outBase) {
		t.Fatalf("wav %q and output base %q not in the same temp dir", wav, outBase)
	}
	// The private temp dir is gone afterwards.
	if _, err := os.Stat(filepath.Dir(wav)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp dir %q left behind (err=%v)", filepath.Dir(wav), err)
	}

	ffmpeg := readArgs(t, f.input+".args")
	if want := ffmpegWAVArgs(f.input, wav, f.cfg.MaxDuration); !slices.Equal(ffmpeg, want) {
		t.Fatalf("ffmpeg args = %v, want %v", ffmpeg, want)
	}
}

func TestTranscribe_FallsBackToStdout(t *testing.T) {
	f := newFakeSetup(t, "stdout=\\n Guten Morgen \\n\n", 2)
	got, err := Transcribe(context.Background(), f.cfg, f.input)
	if err != nil || got != "Guten Morgen" {
		t.Fatalf("Transcribe = %q, %v; want %q", got, err, "Guten Morgen")
	}
}

func TestTranscribe_NoSpeech(t *testing.T) {
	f := newFakeSetup(t, "stdout=  \n", 2)
	if _, err := Transcribe(context.Background(), f.cfg, f.input); !errors.Is(err, ErrNoSpeech) {
		t.Fatalf("err = %v, want ErrNoSpeech", err)
	}
}

// TestTranscribe_NonSpeechMarkersAreNoSpeech: for silence or noise whisper
// prints markers such as [BLANK_AUDIO] instead of nothing. They must end as
// ErrNoSpeech, so nothing is cached or searched as if it were text.
func TestTranscribe_NonSpeechMarkersAreNoSpeech(t *testing.T) {
	// A literal \n in the fake's instructions becomes a line break.
	for _, out := range []string{"[BLANK_AUDIO]", ` [BLANK_AUDIO]\n [BLANK_AUDIO] `, "[ Silence ]", "(music)", "[Musik] (Stille)"} {
		f := newFakeSetup(t, "txt="+out+"\nstdout="+out+"\n", 2)
		if got, err := Transcribe(context.Background(), f.cfg, f.input); !errors.Is(err, ErrNoSpeech) {
			t.Errorf("whisper output %q: Transcribe = %q, %v; want ErrNoSpeech", out, got, err)
		}
	}
}

func TestSpeechText(t *testing.T) {
	cases := map[string]string{
		"":                                  "",
		"  Hallo  Daniel \n":                "Hallo Daniel",
		"[BLANK_AUDIO] Hallo [BLANK_AUDIO]": "Hallo",
		"[blank_audio]":                     "",
		"(Musik) Hallo, hier ist Anna":      "(Musik) Hallo, hier ist Anna",
		"*Applaus*":                         "",
		"Preis [unverständlich] Euro":       "Preis [unverständlich] Euro",
	}
	for in, want := range cases {
		if got := speechText(in); got != want {
			t.Errorf("speechText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTranscribe_Timeout(t *testing.T) {
	f := newFakeSetup(t, "sleep=30s\nstdout=too late\n", 2)
	f.cfg.Timeout = 500 * time.Millisecond
	start := time.Now()
	_, err := Transcribe(context.Background(), f.cfg, f.input)
	if err == nil || !strings.Contains(err.Error(), "timed out after 500ms") || !strings.Contains(err.Error(), EnvWhisperTimeout) {
		t.Fatalf("err = %v, want whisper timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestTranscribe_RejectsAudioOverMaxDuration(t *testing.T) {
	f := newFakeSetup(t, "stdout=should not run\n", 11) // limit is 10 s
	_, err := Transcribe(context.Background(), f.cfg, f.input)
	if !errors.Is(err, ErrAudioTooLong) {
		t.Fatalf("err = %v, want ErrAudioTooLong", err)
	}
	if _, statErr := os.Stat(f.cfg.Model + ".args"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("whisper ran on audio over the limit")
	}
}

func TestTranscribe_WhisperFailureHidesPaths(t *testing.T) {
	f := newFakeSetup(t, `stderr=error: failed to open C:\Users\someone\secret\model.bin and /home/someone/secret.bin`+"\nexit=3\n", 2)
	_, err := Transcribe(context.Background(), f.cfg, f.input)
	if err == nil || !strings.Contains(err.Error(), "whisper failed") {
		t.Fatalf("err = %v, want whisper failure", err)
	}
	if strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "<path>") {
		t.Fatalf("error leaks local paths: %v", err)
	}
}

func TestTranscribe_NotConfigured(t *testing.T) {
	_, err := Transcribe(context.Background(), WhisperConfig{}, "voice.ogg")
	if !errors.Is(err, ErrWhisperNotConfigured) {
		t.Fatalf("err = %v, want ErrWhisperNotConfigured", err)
	}
	for _, want := range []string{EnvWhisperBin, EnvWhisperModel, EnvWhisperLang, EnvWhisperTimeout} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestWhisperConfig_Check(t *testing.T) {
	f := newFakeSetup(t, "", 1)
	if err := f.cfg.Check(); err != nil {
		t.Fatalf("Check on valid config: %v", err)
	}
	missingModel := f.cfg
	missingModel.Model = filepath.Join(f.dir, "nope.bin")
	if err := missingModel.Check(); err == nil || !strings.Contains(err.Error(), EnvWhisperModel) || strings.Contains(err.Error(), f.dir) {
		t.Fatalf("missing model: err = %v", err)
	}
	missingBin := f.cfg
	missingBin.Bin = filepath.Join(f.dir, "no-such-whisper")
	if err := missingBin.Check(); err == nil || !strings.Contains(err.Error(), EnvWhisperBin) {
		t.Fatalf("missing binary: err = %v", err)
	}
}

func TestWhisperConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	cfg, err := WhisperConfigFromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled() || cfg.Lang != "auto" || cfg.Timeout != 5*time.Minute || cfg.MaxDuration != 15*time.Minute {
		t.Fatalf("defaults = %+v", cfg)
	}

	cfg, err = WhisperConfigFromEnv(env(map[string]string{
		EnvWhisperBin:         ` C:\whisper\whisper-cli.exe `,
		EnvWhisperModel:       `C:\whisper\models\ggml-base.bin`,
		EnvWhisperLang:        "DE",
		EnvWhisperTimeout:     "90s",
		EnvWhisperMaxDuration: "600",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := WhisperConfig{
		Bin: `C:\whisper\whisper-cli.exe`, Model: `C:\whisper\models\ggml-base.bin`, Lang: "de",
		Timeout: 90 * time.Second, MaxDuration: 10 * time.Minute,
	}
	if cfg != want || !cfg.Enabled() {
		t.Fatalf("cfg = %+v, want %+v", cfg, want)
	}

	for name, bad := range map[string]map[string]string{
		"language with flag": {EnvWhisperLang: "-x"},
		"language too long":  {EnvWhisperLang: "german"},
		"timeout garbage":    {EnvWhisperTimeout: "soon"},
		"timeout zero":       {EnvWhisperTimeout: "0"},
		"max negative":       {EnvWhisperMaxDuration: "-5m"},
	} {
		if _, err := WhisperConfigFromEnv(env(bad)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestWhisperConfig_CheckDuration(t *testing.T) {
	cfg := WhisperConfig{MaxDuration: time.Minute}
	if err := cfg.CheckDuration(0); err != nil {
		t.Errorf("unknown length rejected: %v", err)
	}
	if err := cfg.CheckDuration(60); err != nil {
		t.Errorf("length at the limit rejected: %v", err)
	}
	if err := cfg.CheckDuration(61); !errors.Is(err, ErrAudioTooLong) {
		t.Errorf("61 s with 1 min limit: err = %v", err)
	}
	if err := (WhisperConfig{}).CheckDuration(15 * 60); err != nil {
		t.Errorf("default limit rejects 15 min: %v", err)
	}
}

func TestFFmpegWAVArgs(t *testing.T) {
	got := ffmpegWAVArgs("-evil.ogg", "out.wav", 15*time.Minute)
	if argAfter(got, "-i") != "./-evil.ogg" {
		t.Errorf("flag-like input not guarded: %v", got)
	}
	if argAfter(got, "-t") != "901" || argAfter(got, "-ar") != "16000" || argAfter(got, "-ac") != "1" || argAfter(got, "-c:a") != "pcm_s16le" {
		t.Errorf("unexpected conversion args: %v", got)
	}
	if got[len(got)-1] != "out.wav" {
		t.Errorf("output must be last: %v", got)
	}
}

func TestWavSeconds(t *testing.T) {
	if got := wavSeconds(44 + 3*wavBytesPerSecond); got != 3 {
		t.Errorf("wavSeconds = %v, want 3", got)
	}
	if got := wavSeconds(10); got != 0 {
		t.Errorf("wavSeconds(header only) = %v, want 0", got)
	}
}
