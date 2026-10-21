package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sealjay/mcp-whatsapp/internal/mediacache"
	"github.com/sealjay/mcp-whatsapp/internal/store"
)

// minRetention keeps a typo such as "-media-older-than 30s" from wiping the
// cache, and keeps a file a running transcription still reads out of reach.
const minRetention = time.Hour

// retentionInterval is how often serve -media-retention prunes after the
// run at startup.
const retentionInterval = 24 * time.Hour

// retentionFlag is a flag.Value for a duration that also accepts whole days
// ("30d") besides Go durations ("720h"). "0" means off.
type retentionFlag struct{ d time.Duration }

func (f *retentionFlag) String() string {
	if f == nil {
		return "0"
	}
	return f.d.String()
}

func (f *retentionFlag) Set(s string) error {
	d, err := parseRetention(s)
	if err != nil {
		return err
	}
	f.d = d
	return nil
}

// parseRetention parses "30d", "720h", "90m" or "0". Zero turns retention
// off; anything else must be at least minRetention.
func parseRetention(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q: use whole days like 30d or a Go duration like 720h", s)
		}
		if n > 100*365 {
			return 0, fmt.Errorf("invalid duration %q: too long", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		d, err = time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: use whole days like 30d or a Go duration like 720h", s)
		}
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid duration %q: must not be negative", s)
	}
	if d > 0 && d < minRetention {
		return 0, fmt.Errorf("invalid duration %q: must be 0 (off) or at least %s", s, minRetention)
	}
	return d, nil
}

// mediaRootFor returns the media root serve uses for absStore:
// WHATSAPP_MCP_MEDIA_ROOT, or <store>/uploads.
func mediaRootFor(absStore string) string {
	root := os.Getenv("WHATSAPP_MCP_MEDIA_ROOT")
	if root == "" {
		root = filepath.Join(absStore, "uploads")
	}
	return filepath.Clean(root)
}

// runPrune deletes downloaded media older than -media-older-than from the
// store. It holds the store lock while it runs, so it refuses to run next to
// a serve on the same store (and serve cannot start meanwhile); a dry run
// only reads and also works while serve runs.
func runPrune(storeDir string, args []string) int {
	var (
		olderThan retentionFlag
		dryRun    bool
	)
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Var(&olderThan, "media-older-than", "delete downloaded media not used for this long, e.g. 30d or 720h (required)")
	fs.BoolVar(&dryRun, "dry-run", false, "only report what would be deleted")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "whatsapp-mcp prune: delete downloaded media files from the store. Messages, transcripts, databases and the uploads directory stay.")
		fmt.Fprintln(os.Stderr, "\nUsage: whatsapp-mcp [-store DIR] prune -media-older-than 30d [-dry-run]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if olderThan.d <= 0 {
		fmt.Fprintln(os.Stderr, "prune: -media-older-than is required, e.g. -media-older-than 30d")
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "prune: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	absStore, err := filepath.Abs(storeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve store dir: %v\n", err)
		return 1
	}
	if info, err := os.Stat(absStore); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "prune: store directory %s not found\n", absStore)
		return 1
	}

	lock, err := store.TryLock(absStore)
	switch {
	case err == nil:
		defer lock.Release()
	case errors.Is(err, store.ErrLocked) && dryRun:
		fmt.Fprintln(os.Stderr, "note: serve is running on this store; the dry run only reads")
	case errors.Is(err, store.ErrLocked):
		fmt.Fprintf(os.Stderr, "prune: %v\nStop serve first, or let serve prune with -media-retention.\n", err)
		return 1
	default:
		fmt.Fprintf(os.Stderr, "prune: %v\n", err)
		return 1
	}

	rep, err := mediacache.Prune(absStore, mediacache.Options{
		OlderThan: olderThan.d,
		DryRun:    dryRun,
		Skip:      []string{mediaRootFor(absStore)},
	})
	printPruneReport(os.Stdout, rep, dryRun)
	if err != nil {
		fmt.Fprintf(os.Stderr, "prune: %v\n", err)
		return 1
	}
	return 0
}

func printPruneReport(w io.Writer, rep mediacache.Report, dryRun bool) {
	verb := "deleted"
	if dryRun {
		verb = "would delete"
	}
	fmt.Fprintf(w, "%s %d media file(s), %s; %d message folder(s) and %d chat folder(s) left empty\n",
		verb, rep.Files, humanBytes(rep.Bytes), rep.MessageDirs, rep.ChatDirs)
	if rep.Ignored > 0 {
		fmt.Fprintf(w, "left alone: %d entries that do not belong to the media cache layout\n", rep.Ignored)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// mediaRetentionLoop calls prune once right away and then every interval
// until ctx ends.
func mediaRetentionLoop(ctx context.Context, interval time.Duration, prune func()) {
	prune()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		}
	}
}
