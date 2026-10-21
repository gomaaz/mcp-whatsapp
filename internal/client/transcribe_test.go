package client

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"

	"github.com/sealjay/mcp-whatsapp/internal/media"
	"github.com/sealjay/mcp-whatsapp/internal/store"
)

func TestExtractAudioInfo(t *testing.T) {
	cases := []struct {
		name      string
		msg       *waProto.Message
		seconds   uint32
		voiceNote bool
	}{
		{"nil", nil, 0, false},
		{"text", &waProto.Message{Conversation: proto.String("hi")}, 0, false},
		{"image", &waProto.Message{ImageMessage: &waProto.ImageMessage{}}, 0, false},
		{"voice note", &waProto.Message{AudioMessage: &waProto.AudioMessage{Seconds: proto.Uint32(45), PTT: proto.Bool(true)}}, 45, true},
		{"audio file", &waProto.Message{AudioMessage: &waProto.AudioMessage{Seconds: proto.Uint32(3)}}, 3, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			secs, ptt := extractAudioInfo(tc.msg)
			if secs != tc.seconds || ptt != tc.voiceNote {
				t.Fatalf("extractAudioInfo = (%d, %v), want (%d, %v)", secs, ptt, tc.seconds, tc.voiceNote)
			}
		})
	}
}

func TestNormalizeIncomingMessage_VoiceNote(t *testing.T) {
	s := newTestStoreWithLIDMap(t, nil)
	c := newClientWithStore(t, s)
	nm := c.normalizeIncomingMessage(
		"447700000001@s.whatsapp.net", "447700000001@s.whatsapp.net", "447700000001",
		false, false, "", "voice-1",
		time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		&waProto.Message{AudioMessage: &waProto.AudioMessage{Seconds: proto.Uint32(45), PTT: proto.Bool(true)}},
	)
	if nm.msg.MediaType != "audio" || nm.msg.AudioSeconds != 45 || !nm.msg.IsVoiceNote {
		t.Fatalf("got media=%q seconds=%d voice=%v, want audio/45/true", nm.msg.MediaType, nm.msg.AudioSeconds, nm.msg.IsVoiceNote)
	}
}

// transcribeFixture is a store with one cached voice note whose payload the
// fake downloader serves, and a client wired to a counting fake transcriber.
type transcribeFixture struct {
	c           *Client
	s           *store.Store
	downloads   int
	transcribes int
}

const (
	trJID   = "447700000099@s.whatsapp.net"
	trMsgID = "VOICE-1"
)

func newTranscribeFixture(t *testing.T, seconds uint32) *transcribeFixture {
	t.Helper()
	t.Chdir(t.TempDir())
	s, err := store.Open("./store")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	payload := []byte("OggS-fake-voice-note")
	digest := sha256.Sum256(payload)
	if err := s.StoreChat(trJID, "Kunde", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage(context.Background(), store.Message{
		ID: trMsgID, ChatJID: trJID, Sender: "447700000099", Timestamp: time.Now().UTC(),
		MediaType: "audio", Filename: "audio_x.ogg", URL: "https://example.invalid/a", DirectPath: "/a",
		AudioSeconds: seconds, IsVoiceNote: true,
	}, []byte{1}, digest[:], []byte{2}, uint64(len(payload))); err != nil {
		t.Fatal(err)
	}

	f := &transcribeFixture{s: s}
	f.c = newClientWithStore(t, s)
	f.c.whisper = media.WhisperConfig{Bin: "whisper-cli", Model: "ggml-base.bin"}
	f.c.downloadMedia = func(context.Context, whatsmeow.DownloadableMessage) ([]byte, error) {
		f.downloads++
		return payload, nil
	}
	f.c.transcribe = func(_ context.Context, _ media.WhisperConfig, path string) (string, error) {
		f.transcribes++
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if string(data) != string(payload) {
			return "", errors.New("transcriber got the wrong file")
		}
		return "Hallo, ich brauche ein Angebot", nil
	}
	return f
}

func TestTranscribeMedia_TranscribesOnceThenServesCache(t *testing.T) {
	f := newTranscribeFixture(t, 45)
	ctx := context.Background()

	first := f.c.TranscribeMedia(ctx, trMsgID, trJID)
	if !first.Success || first.Cached || first.Transcript != "Hallo, ich brauche ein Angebot" {
		t.Fatalf("first call = %+v", first)
	}
	if first.AudioSeconds != 45 || !first.IsVoiceNote {
		t.Fatalf("first call audio info = %d/%v, want 45/true", first.AudioSeconds, first.IsVoiceNote)
	}

	second := f.c.TranscribeMedia(ctx, trMsgID, trJID)
	if !second.Success || !second.Cached || second.Transcript != first.Transcript {
		t.Fatalf("second call = %+v, want cached transcript", second)
	}
	if f.downloads != 1 || f.transcribes != 1 {
		t.Fatalf("downloads=%d transcribes=%d, want 1/1", f.downloads, f.transcribes)
	}

	// The cached transcript is on the list_messages line and searchable.
	msgs, err := f.s.ListMessages(ctx, store.ListMessagesParams{Query: "angebot"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ID != trMsgID {
		t.Fatalf("query on transcript returned %+v", msgs)
	}
	line := f.s.FormatMessage(ctx, msgs[0], false)
	if !strings.Contains(line, `[voice note, 45 s] transcript: "Hallo, ich brauche ein Angebot"`) {
		t.Fatalf("formatted line = %q", line)
	}
}

func TestTranscribeMedia_CachedTranscriptNeedsNoWhisper(t *testing.T) {
	f := newTranscribeFixture(t, 45)
	if _, err := f.s.SetTranscript(context.Background(), trMsgID, trJID, "schon da"); err != nil {
		t.Fatal(err)
	}
	f.c.whisper = media.WhisperConfig{}
	got := f.c.TranscribeMedia(context.Background(), trMsgID, trJID)
	if !got.Success || !got.Cached || got.Transcript != "schon da" {
		t.Fatalf("got %+v, want cached transcript without whisper", got)
	}
}

func TestTranscribeMedia_NotConfigured(t *testing.T) {
	f := newTranscribeFixture(t, 45)
	f.c.whisper = media.WhisperConfig{}
	got := f.c.TranscribeMedia(context.Background(), trMsgID, trJID)
	if got.Success {
		t.Fatalf("expected failure, got %+v", got)
	}
	for _, want := range []string{media.EnvWhisperBin, media.EnvWhisperModel} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message %q does not name %s", got.Message, want)
		}
	}
	if f.downloads != 0 || f.transcribes != 0 {
		t.Fatalf("downloads=%d transcribes=%d, want nothing run", f.downloads, f.transcribes)
	}
}

func TestTranscribeMedia_TooLongIsRejectedBeforeDownload(t *testing.T) {
	f := newTranscribeFixture(t, 16*60)
	got := f.c.TranscribeMedia(context.Background(), trMsgID, trJID)
	if got.Success || !strings.Contains(got.Message, "longer than the transcription limit") {
		t.Fatalf("got %+v, want too-long error", got)
	}
	if f.downloads != 0 {
		t.Fatalf("downloaded %d times, want 0", f.downloads)
	}
}

func TestTranscribeMedia_FailureIsNotCached(t *testing.T) {
	f := newTranscribeFixture(t, 45)
	f.c.transcribe = func(context.Context, media.WhisperConfig, string) (string, error) {
		f.transcribes++
		return "", errors.New("whisper timed out after 5m0s")
	}
	got := f.c.TranscribeMedia(context.Background(), trMsgID, trJID)
	if got.Success || !strings.Contains(got.Message, "timed out") {
		t.Fatalf("got %+v, want timeout failure", got)
	}
	info, err := f.s.GetAudioInfo(context.Background(), trMsgID, trJID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Transcript != "" {
		t.Fatalf("failed transcription cached %q", info.Transcript)
	}
}

func TestTranscribeMedia_RejectsNonAudioAndUnknown(t *testing.T) {
	f := newTranscribeFixture(t, 45)
	ctx := context.Background()
	if err := f.s.StoreMessage(ctx, store.Message{
		ID: "TEXT-1", ChatJID: trJID, Sender: "447700000099", Content: "hallo", Timestamp: time.Now().UTC(),
	}, nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if got := f.c.TranscribeMedia(ctx, "TEXT-1", trJID); got.Success || got.Message != "not an audio message" {
		t.Fatalf("text message: %+v", got)
	}
	if got := f.c.TranscribeMedia(ctx, "NOPE", trJID); got.Success || !strings.Contains(got.Message, "not found") {
		t.Fatalf("unknown message: %+v", got)
	}
	if f.downloads != 0 || f.transcribes != 0 {
		t.Fatalf("downloads=%d transcribes=%d, want 0/0", f.downloads, f.transcribes)
	}
}
