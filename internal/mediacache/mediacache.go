// Package mediacache owns the layout of the downloaded-media cache under the
// store directory: <store>/<chat_jid with ':' as '_'>/<hex(message_id)>/<file>.
// download_media writes there; Prune and RemoveMessage delete from there.
//
// Deletion only ever touches that layout. Top-level entries must be real
// directories named like a chat JID (they contain '@'), the next level real
// directories named by a lowercase hex message ID, and only regular files
// inside those are removed. messages.db, whatsapp.db, .lock, the uploads
// directory and anything else in the store are never entered. Symlinks and
// junctions are not followed, and every directory is checked to resolve
// inside the store before anything in it is deleted.
//
// Errors from Prune and RemoveMessage carry no paths: chat directories are
// named after the chat JID (a phone number), message directories after the
// hex-encoded message ID, and files after the attachment, and these errors
// end up in the serve log.
package mediacache

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sealjay/mcp-whatsapp/internal/security"
)

// ErrEscapesStore is returned by MessageDir when a chat JID would place the
// cache directory outside the store.
var ErrEscapesStore = errors.New("invalid chat directory: path escapes store")

// MessageDir returns the absolute cache directory of one message. It does
// not create it. A chat JID that would resolve outside storeDir (for example
// one containing "..") returns ErrEscapesStore.
func MessageDir(storeDir, chatJID, messageID string) (string, error) {
	root, err := filepath.Abs(storeDir)
	if err != nil {
		return "", fmt.Errorf("resolve store dir: %w", err)
	}
	root = filepath.Clean(root)
	chatDir := filepath.Clean(filepath.Join(root, chatDirName(chatJID)))
	if filepath.Dir(chatDir) != root {
		return "", ErrEscapesStore
	}
	return filepath.Join(chatDir, hex.EncodeToString([]byte(messageID))), nil
}

// chatDirName is the directory name of a chat's cache. Colons (device JIDs)
// are not valid in Windows file names.
func chatDirName(chatJID string) string {
	return strings.ReplaceAll(chatJID, ":", "_")
}

// Options configure Prune.
type Options struct {
	// OlderThan is the minimum age of a file to delete, measured from its
	// modification time. download_media refreshes that time on every cache
	// hit, so it is the time since the file was last fetched or used.
	OlderThan time.Duration
	// Now is the reference time; zero means time.Now().
	Now time.Time
	// DryRun reports what would be deleted without deleting anything.
	DryRun bool
	// Skip lists directories that are never entered, such as the media root
	// that download_media's output_path writes to. A skip directory that is
	// the store itself or contains it is an error.
	Skip []string
}

// Report says what Prune deleted, or would delete in a dry run.
type Report struct {
	Files       int   // media files
	Bytes       int64 // their total size
	MessageDirs int   // message directories left empty and removed
	ChatDirs    int   // chat directories left empty and removed
	// Ignored counts entries inside chat or message directories that do not
	// match the cache layout (symlinks, unexpected names) and were left
	// alone.
	Ignored int
}

// Prune deletes downloaded media files older than opts.OlderThan from the
// cache under storeDir, then removes message and chat directories that are
// left empty. It never touches the databases, the lock file or anything
// outside the cache layout (see the package comment). Files it cannot
// delete, for example because another program has them open, are reported
// in the returned error; the rest of the tree is still pruned.
//
// Prune does not coordinate with a running download. The caller must make
// sure nothing writes to the cache meanwhile: the serve daemon holds its
// media lock, and the prune subcommand refuses to run while serve holds the
// store lock.
func Prune(storeDir string, opts Options) (Report, error) {
	var rep Report
	if opts.OlderThan <= 0 {
		return rep, errors.New("prune: age must be positive")
	}
	abs, err := filepath.Abs(storeDir)
	if err != nil {
		return rep, fmt.Errorf("prune: resolve store dir: %w", err)
	}
	root, err := filepath.EvalSymlinks(filepath.Clean(abs))
	if err != nil {
		return rep, fmt.Errorf("prune: store dir: %w", err)
	}
	skips, err := resolveSkips(root, opts.Skip)
	if err != nil {
		return rep, err
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	p := pruner{root: root, skips: skips, cutoff: now.Add(-opts.OlderThan), dryRun: opts.DryRun, rep: &rep}

	entries, err := os.ReadDir(root)
	if err != nil {
		return rep, fmt.Errorf("prune: read store dir: %w", err)
	}
	for _, e := range entries {
		// Top-level files (databases, .lock) and directories that are not
		// named like a chat (uploads) are not part of the cache.
		if !isPlainDir(e) || !isChatDirName(e.Name()) {
			continue
		}
		p.chat(filepath.Join(root, e.Name()))
	}
	return rep, failuresError("prune", p.errs)
}

type pruner struct {
	root   string
	skips  []string
	cutoff time.Time
	dryRun bool
	rep    *Report
	errs   []error
}

// chat prunes one chat directory and removes it once no message directory
// is left in it.
func (p *pruner) chat(dir string) {
	if !p.safeDir(dir) {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		p.errs = append(p.errs, pathFree("read chat folder", err))
		return
	}
	left := len(entries)
	for _, e := range entries {
		if !isPlainDir(e) || !isMessageDirName(e.Name()) {
			p.rep.Ignored++
			continue
		}
		if p.message(filepath.Join(dir, e.Name())) {
			left--
		}
	}
	if left == 0 && p.removeDir(dir) {
		p.rep.ChatDirs++
	}
}

// message prunes one message directory and reports whether it is (or would
// be) removed because nothing is left in it.
func (p *pruner) message(dir string) bool {
	if !p.safeDir(dir) {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		p.errs = append(p.errs, pathFree("read message folder", err))
		return false
	}
	left := len(entries)
	for _, e := range entries {
		if !e.Type().IsRegular() {
			p.rep.Ignored++
			continue
		}
		info, err := e.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				p.errs = append(p.errs, pathFree("inspect file", err))
			}
			continue
		}
		if !info.ModTime().Before(p.cutoff) {
			continue
		}
		if !p.dryRun {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				p.errs = append(p.errs, pathFree("delete file", err))
				continue
			}
		}
		p.rep.Files++
		p.rep.Bytes += info.Size()
		left--
	}
	if left == 0 && p.removeDir(dir) {
		p.rep.MessageDirs++
		return true
	}
	return false
}

// safeDir reports whether dir may be pruned: it resolves inside the store
// and is not a skipped directory or one that contains it.
func (p *pruner) safeDir(dir string) bool {
	resolved, err := security.ResolveWithin(dir, p.root)
	if err != nil || resolved == p.root {
		p.rep.Ignored++
		return false
	}
	for _, s := range p.skips {
		if isWithin(resolved, s) || isWithin(s, resolved) {
			p.rep.Ignored++
			return false
		}
	}
	return true
}

// removeDir removes an empty directory; a dry run only reports success.
func (p *pruner) removeDir(dir string) bool {
	if p.dryRun {
		return true
	}
	if err := os.Remove(dir); err != nil {
		p.errs = append(p.errs, pathFree("delete empty folder", err))
		return false
	}
	return true
}

// RemoveMessage deletes the cached files of one message, its directory once
// empty, and the chat directory if that is empty afterwards. A message
// without cached files is not an error. It reports how many files it
// deleted. The same layout and symlink rules as Prune apply: only regular
// files are removed, and a directory that is a symlink or resolves outside
// the store is left alone with an error.
func RemoveMessage(storeDir, chatJID, messageID string) (int, error) {
	dir, err := MessageDir(storeDir, chatJID, messageID)
	if err != nil {
		return 0, err
	}
	chatDir := filepath.Dir(dir)
	for _, d := range []string{chatDir, dir} {
		info, err := os.Lstat(d)
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		if err != nil {
			return 0, pathFree("inspect media cache folder", err)
		}
		if info.Mode().Type() != fs.ModeDir {
			return 0, errors.New("media cache folder is not a plain directory")
		}
		if _, err := security.ResolveWithin(d, storeDir); err != nil {
			return 0, errors.New("media cache folder resolves outside the store or cannot be resolved")
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, pathFree("read message folder", err)
	}
	removed, left := 0, len(entries)
	var errs []error
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, pathFree("delete file", err))
			continue
		}
		removed++
		left--
	}
	if left == 0 {
		if err := os.Remove(dir); err != nil {
			errs = append(errs, pathFree("delete empty folder", err))
		} else {
			// Only succeeds when no other message of the chat is cached.
			_ = os.Remove(chatDir)
		}
	}
	return removed, failuresError("remove cached media", errs)
}

// pathFree reduces a file system error to what went wrong, without the
// path: "delete file: The process cannot access the file because it is
// being used by another process." The underlying error stays wrapped, so
// errors.Is(err, fs.ErrPermission) still works. An error of another type
// may carry a path in its text and is replaced by "<op> failed".
func pathFree(op string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", op, pe.Err)
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return fmt.Errorf("%s: %w", op, le.Err)
	}
	var se *os.SyscallError
	if errors.As(err, &se) {
		return fmt.Errorf("%s: %w", op, se.Err)
	}
	return fmt.Errorf("%s failed", op)
}

// failures is the error Prune and RemoveMessage return when entries could
// not be read or deleted: how many failed and each distinct reason once, on
// one line.
type failures struct {
	op      string
	count   int
	reasons []error
}

func (f *failures) Error() string {
	msgs := make([]string, len(f.reasons))
	for i, r := range f.reasons {
		msgs[i] = r.Error()
	}
	return fmt.Sprintf("%s: %d cache entr%s failed: %s", f.op, f.count, plural(f.count, "y", "ies"), strings.Join(msgs, "; "))
}

func (f *failures) Unwrap() []error { return f.reasons }

// failuresError returns nil for no errors, otherwise a *failures with the
// distinct reasons of errs.
func failuresError(op string, errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	f := &failures{op: op, count: len(errs)}
	seen := make(map[string]bool)
	for _, err := range errs {
		if msg := err.Error(); !seen[msg] {
			seen[msg] = true
			f.reasons = append(f.reasons, err)
		}
	}
	return f
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// resolveSkips resolves the skip directories; one that does not exist yet
// is kept in its cleaned absolute form.
func resolveSkips(root string, skips []string) ([]string, error) {
	out := make([]string, 0, len(skips))
	for _, s := range skips {
		if s == "" {
			continue
		}
		abs, err := filepath.Abs(s)
		if err != nil {
			return nil, fmt.Errorf("prune: resolve %q: %w", s, err)
		}
		abs = filepath.Clean(abs)
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			abs = r
		}
		if isWithin(root, abs) {
			return nil, fmt.Errorf("prune: %q contains the store directory; refusing to prune", abs)
		}
		out = append(out, abs)
	}
	return out, nil
}

// isPlainDir reports whether e is a directory and not a symlink, junction
// or other reparse point.
func isPlainDir(e fs.DirEntry) bool {
	return e.Type() == fs.ModeDir
}

// isChatDirName reports whether name looks like a chat cache directory.
func isChatDirName(name string) bool {
	return strings.Contains(name, "@") && !strings.HasPrefix(name, ".")
}

// isMessageDirName reports whether name is a hex-encoded message ID as
// MessageDir writes it.
func isMessageDirName(name string) bool {
	if name == "" || name != strings.ToLower(name) {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

func isWithin(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}
