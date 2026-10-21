package client

import (
	"context"
	"errors"
	"fmt"

	"github.com/sealjay/mcp-whatsapp/internal/media"
	"github.com/sealjay/mcp-whatsapp/internal/store"
)

// TranscribeResult is the public return value from TranscribeMedia.
type TranscribeResult struct {
	Success    bool
	Message    string
	Transcript string `json:",omitempty"`
	// Cached is true when the transcript came from the local cache and
	// nothing was downloaded or run.
	Cached       bool
	AudioSeconds uint32 `json:",omitempty"`
	IsVoiceNote  bool
}

// TranscribeMedia returns the text of a cached audio message. A transcript
// cached by an earlier call is returned as is. Otherwise the audio is fetched
// through Download (the same path as download_media, which sends nothing to
// the chat), transcribed locally with whisper.cpp and cached, so list_messages
// shows and searches it from then on.
func (c *Client) TranscribeMedia(ctx context.Context, messageID, chatJID string) TranscribeResult {
	info, err := c.store.GetAudioInfo(ctx, messageID, chatJID)
	if errors.Is(err, store.ErrMessageNotFound) {
		return TranscribeResult{Message: "message not found in cache; use list_messages to find message_id and chat_jid"}
	}
	if err != nil {
		return TranscribeResult{Message: err.Error()}
	}
	res := TranscribeResult{AudioSeconds: info.AudioSeconds, IsVoiceNote: info.IsVoiceNote}
	if info.MediaType != "audio" {
		res.Message = "not an audio message"
		if info.MediaType != "" {
			res.Message += " (media type " + info.MediaType + ")"
		}
		return res
	}
	if info.Transcript != "" {
		res.Success = true
		res.Cached = true
		res.Transcript = info.Transcript
		res.Message = "Transcript from cache"
		return res
	}
	if info.Revoked && !c.keepRevoked {
		res.Message = "message was deleted by its sender; its audio is no longer stored"
		return res
	}
	if !c.whisper.Enabled() {
		res.Message = media.ErrWhisperNotConfigured.Error()
		return res
	}
	if err := c.whisper.CheckDuration(info.AudioSeconds); err != nil {
		res.Message = err.Error()
		return res
	}

	c.transcribeMu.Lock()
	defer c.transcribeMu.Unlock()

	// A call that waited for the lock may find the work already done.
	if again, err := c.store.GetAudioInfo(ctx, messageID, chatJID); err == nil && again.Transcript != "" {
		res.Success = true
		res.Cached = true
		res.Transcript = again.Transcript
		res.Message = "Transcript from cache"
		return res
	}

	dl := c.Download(ctx, messageID, chatJID, "")
	if !dl.Success {
		res.Message = dl.Message
		return res
	}

	transcribe := c.transcribe
	if transcribe == nil {
		transcribe = media.Transcribe
	}
	text, err := transcribe(ctx, c.whisper, dl.Path)
	if err != nil {
		res.Message = fmt.Sprintf("transcription failed: %v", err)
		return res
	}

	if _, err := c.store.SetTranscript(ctx, messageID, chatJID, text); err != nil {
		c.log.Warnf("Failed to cache transcript for message %s: %v", c.redactor.MsgID(messageID), err)
	}
	c.log.Infof("Transcribed audio message %s (%d s)", c.redactor.MsgID(messageID), info.AudioSeconds)
	res.Success = true
	res.Transcript = text
	res.Message = "Transcribed locally with whisper.cpp"
	return res
}
