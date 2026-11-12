// Package session persists conversations as JSONL, one entry per line with
// parent links. sal keeps the tree but only uses the linear active branch.
package session

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bothought/sal/internal/agent"
	"github.com/bothought/sal/internal/config"
)

// RootWith returns the canonical session storage root: an explicit directory
// (--session-dir or the config file), then $SAL_SESSION_DIR, then the XDG
// data directory. New sessions are always written here.
func RootWith(dir string) string {
	if strings.TrimSpace(dir) != "" {
		return config.ExpandPath(dir)
	}
	if v := strings.TrimSpace(os.Getenv("SAL_SESSION_DIR")); v != "" {
		return config.ExpandPath(v)
	}
	return config.Resolve().SessionDir
}

// canonicalDir resolves a working directory to the path that identifies it, so
// the same directory always lands in the same bucket.
//
// An absolute path is not enough: a path can be reached through a symbolic link
// (macOS reaches temporary directories through /private/var, and a person may
// keep a project behind a link), and the two spellings would otherwise become
// two buckets. Resolving first makes the identity physical. A path that cannot
// be resolved is used as written.
func canonicalDir(cwd string) string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// ProjectDir is the per-working-directory bucket, derived from a canonical cwd
// so sessions are grouped by project.
//
// The slug has to be usable as a directory name on every platform the program
// runs on, so it is built by an explicit rule rather than by replacing
// separators: a Windows path starts with a drive ("C:\work"), and the colon is
// illegal in a file name there, so a plain separator replacement would create a
// bucket that cannot be written.
func ProjectDir(root, cwd string) string {
	abs := canonicalDir(cwd)
	var b strings.Builder
	for _, r := range abs {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			// Separators, drive colons and anything else a file name cannot
			// carry all become the same separator, so the bucket stays one
			// directory.
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "root"
	}
	return filepath.Join(root, slug)
}

// Entry is one line of a session file.
type Entry struct {
	Type      string         `json:"type"` // session | message | model_change | name | compaction
	ID        string         `json:"id"`
	ParentID  string         `json:"parentId,omitempty"`
	Timestamp string         `json:"timestamp"`
	SessionID string         `json:"sessionId,omitempty"`
	Cwd       string         `json:"cwd,omitempty"`
	Model     string         `json:"model,omitempty"`
	Name      string         `json:"name,omitempty"`
	Message   *agent.Message `json:"message,omitempty"`
	// Compacted records a context compaction; see Compaction.
	Compacted *Compaction `json:"compacted,omitempty"`
}

// Compaction is a recorded compaction: the summary that replaced a prefix of
// the conversation, and which messages it covers. The messages themselves stay
// in the file, so the log remains a complete record and only the active context
// is smaller.
type Compaction struct {
	Summary string `json:"summary"`
	// Through is the id of the last message the summary replaces. It is an id
	// rather than an index so it survives any change in how messages are
	// counted.
	Through  string `json:"through"`
	Messages int    `json:"messages"`
	// Shortened is how many tool results in the kept tail were replaced with a
	// marker, and ShortenedIDs names the messages they were, so a resumed
	// session sends the same context the running one did.
	Shortened    int      `json:"shortened,omitempty"`
	ShortenedIDs []string `json:"shortenedIds,omitempty"`
}

// Info summarises a stored session.
type Info struct {
	Path      string
	ID        string
	Name      string
	Cwd       string
	Model     string
	Messages  int
	UpdatedAt time.Time
	// Compacted reports that the session has a recorded compaction, so a
	// switcher can show why it is shorter than its file.
	Compacted bool
}

// Session is an append-only conversation log.
type Session struct {
	ID       string
	Name     string
	Cwd      string
	Model    string
	Path     string
	Messages []agent.Message
	// Compaction is the recorded compaction, or nil when the context is the
	// whole conversation.
	Compaction *Compaction
	// UpdatedAt is the timestamp of the newest entry, used to order sessions.
	UpdatedAt time.Time
	// recorded reports that a session header was read from the file. A file
	// without one is not a conversation, only a leftover from an interrupted
	// creation.
	recorded bool

	mu         sync.Mutex
	lastID     string
	file       *os.File
	messageIDs []string
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// The names inside a project bucket. A directory's conversation always has the
// fixed name; a conversation moved aside by --new keeps a stamp instead.
const (
	sessionName   = "session.jsonl"
	archivedStart = "session-"
)

// Path is the file a directory's conversation lives in. A directory has
// exactly one, so the name carries no timestamp or id.
func Path(root, cwd string) string {
	return filepath.Join(ProjectDir(root, cwd), sessionName)
}

// New creates and persists a fresh session in the given root directory. An
// existing conversation there is not touched; callers that mean "start over"
// move it aside first (see Archive).
//
// The file is claimed exclusively, so two processes starting in the same
// directory at the same moment cannot both become its conversation: the loser
// is told the file exists and opens what the winner created (see Open). The
// header follows immediately; a reader that finds the file before it is written
// waits for it, which is why no path reports a conversation without one.
func New(root, cwd, name, model string) (*Session, error) {
	dir := ProjectDir(root, cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	abs := canonicalDir(cwd)
	id := newID()
	path := Path(root, cwd)

	header := Entry{Type: "session", ID: newID(), Timestamp: now(), SessionID: id, Cwd: abs, Model: model, Name: name}
	data, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		// Nothing was recorded, so the name is not a conversation: give it back
		// rather than leave a file that reads as a header-less leftover.
		_ = os.Remove(path)
		return nil, err
	}

	s := &Session{ID: id, Name: name, Cwd: abs, Model: model, Path: path, file: f, lastID: header.ID, recorded: true}
	if ts, perr := time.Parse(time.RFC3339Nano, header.Timestamp); perr == nil {
		s.UpdatedAt = ts
	}
	return s, nil
}

func (s *Session) open() error {
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	s.file = f
	return nil
}

func (s *Session) write(e Entry) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ID == "" {
		e.ID = newID()
	}
	if e.ParentID == "" {
		e.ParentID = s.lastID
	}
	if e.Timestamp == "" {
		e.Timestamp = now()
	}
	if s.file == nil {
		if err := s.open(); err != nil {
			return "", err
		}
	}
	data, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	if _, err := s.file.Write(append(data, '\n')); err != nil {
		return "", err
	}
	if ts, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil {
		s.UpdatedAt = ts
	} else {
		s.UpdatedAt = time.Now()
	}
	s.lastID = e.ID
	return e.ID, nil
}

// AppendMessage records a conversation message and keeps the in-memory history.
func (s *Session) AppendMessage(m agent.Message) error {
	msg := m
	s.Messages = append(s.Messages, msg)
	if s.file == nil {
		return nil // ephemeral modes (-p/--no-session) may have no file
	}
	id, err := s.write(Entry{Type: "message", Message: &msg})
	if err != nil {
		return err
	}
	s.messageIDs = append(s.messageIDs, id)
	return nil
}

// MessageID returns the stored id of the message at index i, or "" when it is
// unknown (an ephemeral session, or an index outside the conversation). It is
// what a compaction records so the summary can be tied back to the messages it
// covers.
func (s *Session) MessageID(i int) string {
	if i < 0 || i >= len(s.messageIDs) {
		return ""
	}
	return s.messageIDs[i]
}

// MessageIDs is MessageID for a list of indexes, skipping the ones it cannot
// resolve, so a compaction can record a set of messages by identity.
func (s *Session) MessageIDs(indexes []int) []string {
	if len(indexes) == 0 {
		return nil
	}
	out := make([]string, 0, len(indexes))
	for _, i := range indexes {
		if id := s.MessageID(i); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// CompactionIndex is the index in Messages of the last message a recorded
// compaction covers, or -1 when there is no compaction or it cannot be located.
func (s *Session) CompactionIndex() int {
	if s.Compaction == nil || s.Compaction.Through == "" {
		return -1
	}
	return s.indexByID(s.Compaction.Through)
}

// CompactionShortenedIndexes maps the recorded shortened tool results back to
// their positions in Messages, skipping any whose id is no longer present.
func (s *Session) CompactionShortenedIndexes() []int {
	if s.Compaction == nil || len(s.Compaction.ShortenedIDs) == 0 {
		return nil
	}
	out := make([]int, 0, len(s.Compaction.ShortenedIDs))
	for _, id := range s.Compaction.ShortenedIDs {
		if i := s.indexByID(id); i >= 0 {
			out = append(out, i)
		}
	}
	return out
}

// indexByID is the position in Messages of the message with the given stored
// id, or -1.
func (s *Session) indexByID(id string) int {
	for i, got := range s.messageIDs {
		if got == id {
			return i
		}
	}
	return -1
}

// SetName records a new display name.
func (s *Session) SetName(name string) error {
	s.Name = name
	if s.file == nil {
		return nil
	}
	_, err := s.write(Entry{Type: "name", Name: name})
	return err
}

// AppendCompaction records that a summary replaced a prefix of the
// conversation. The messages it covers stay in the file; this entry is what
// makes the active context smaller on resume. throughID is the id of the last
// message the summary replaces.
func (s *Session) AppendCompaction(c Compaction, throughID string) error {
	if c.Through == "" {
		c.Through = throughID
	}
	s.Compaction = &c
	if s.file == nil {
		return nil
	}
	entry := c
	_, err := s.write(Entry{Type: "compaction", Compacted: &entry})
	return err
}

// SetModel records a model switch.
func (s *Session) SetModel(model string) error {
	s.Model = model
	if s.file == nil {
		return nil
	}
	_, err := s.write(Entry{Type: "model_change", Model: model})
	return err
}

// Close flushes and closes the session file.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// Open returns the directory's conversation, creating it when there is none.
//
// A directory has one conversation, so this is the only way sal starts: there
// is no list to choose from and no "continue" flag. A file written by an older
// release — many timestamped files per directory — is adopted rather than
// migrated: the newest becomes the conversation, and the rest stay on disk.
func Open(root, cwd, name, model string) (*Session, bool, error) {
	path := Path(root, cwd)
	if _, err := os.Stat(path); err == nil {
		s, err := Load(path)
		if err != nil {
			return nil, false, err
		}
		if !s.recorded {
			// The file holds no header. Either another sal claimed it a moment
			// ago and is writing the header, or an interrupted write left it
			// behind. A fresh file is given time to gain its header; an older
			// one is a leftover, not a conversation, and is replaced rather than
			// resumed as an id printed from a file name.
			if fi, serr := os.Stat(path); serr == nil && time.Since(fi.ModTime()) < headerWait {
				if s, ok := waitForHeader(path); ok {
					if err := s.open(); err != nil {
						return nil, false, err
					}
					return s, true, nil
				}
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return nil, false, err
			}
		} else {
			if err := s.open(); err != nil {
				return nil, false, err
			}
			return s, true, nil
		}
	}
	// Adopt the newest file the old layout left in this directory's bucket. An
	// archived conversation is excluded: starting over must not adopt what it
	// just moved aside.
	if legacy, err := newestLegacy(ProjectDir(root, cwd)); err == nil && legacy != "" {
		if err := os.Rename(legacy, path); err == nil {
			if s, lerr := Load(path); lerr == nil {
				if oerr := s.open(); oerr == nil {
					return s, true, nil
				}
			}
		}
	}
	s, err := New(root, cwd, name, model)
	if err != nil {
		// A concurrent sal in the same directory won the creation. Its file is
		// the directory's conversation now, so open that instead of failing.
		if errors.Is(err, fs.ErrExist) {
			if s, ok := waitForHeader(path); ok {
				if oerr := s.open(); oerr != nil {
					return nil, false, oerr
				}
				return s, true, nil
			}
		}
		return nil, false, err
	}
	return s, false, nil
}

// headerWait is how long a just-created file is given to gain its header, and
// how old a header-less file must be before it counts as a leftover. Writing
// the header follows the claim immediately, so this only ever waits out a
// concurrent creation.
const headerWait = 2 * time.Second

// waitForHeader reads path until it holds a session header, reporting the
// session and whether one arrived before headerWait elapsed.
func waitForHeader(path string) (*Session, bool) {
	deadline := time.Now().Add(headerWait)
	for {
		if s, err := Load(path); err == nil && s.recorded {
			return s, true
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// newestLegacy returns the most recently written session file in dir that is not
// the current layout's, or "".
func newestLegacy(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var newest string
	var newestAt time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") || e.Name() == sessionName {
			continue
		}
		// Archived conversations are deliberately left out: --new must not be
		// undone by the next open.
		if strings.HasPrefix(e.Name(), archivedStart) {
			continue
		}
		info, serr := stat(filepath.Join(dir, e.Name()))
		if serr != nil || info.ID == "" {
			// Unreadable, or a leftover with no session header in it.
			continue
		}
		if newest == "" || info.UpdatedAt.After(newestAt) {
			newest, newestAt = info.Path, info.UpdatedAt
		}
	}
	return newest, nil
}

// Archive moves the directory's conversation out of the way and returns where it
// went, so starting over is reversible. It reports "" when there was nothing to
// move.
func Archive(root, cwd string) (string, error) {
	path := Path(root, cwd)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	dir := filepath.Dir(path)
	stamp := time.Now().Format("20060102-150405")
	dst := filepath.Join(dir, archivedStart+stamp+".jsonl")
	for i := 1; ; i++ {
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			break
		}
		dst = filepath.Join(dir, fmt.Sprintf("%s%s-%d.jsonl", archivedStart, stamp, i))
	}
	if err := os.Rename(path, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// Load reads a session file and reconstructs its active (linear) branch.
func Load(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	s := &Session{Path: path}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // tolerate a truncated final line
		}
		switch e.Type {
		case "session":
			s.ID = e.SessionID
			s.Cwd = e.Cwd
			s.Model = e.Model
			s.Name = e.Name
			s.recorded = true
		case "message":
			if e.Message != nil {
				s.Messages = append(s.Messages, *e.Message)
				s.messageIDs = append(s.messageIDs, e.ID)
			}
		case "name":
			s.Name = e.Name
		case "model_change":
			s.Model = e.Model
		case "compaction":
			if e.Compacted != nil {
				c := *e.Compacted
				s.Compaction = &c
			}
		}
		if e.ID != "" {
			s.lastID = e.ID
		}
		if e.Timestamp != "" {
			if ts, perr := time.Parse(time.RFC3339Nano, e.Timestamp); perr == nil && ts.After(s.UpdatedAt) {
				s.UpdatedAt = ts
			}
		}
	}
	if s.ID == "" {
		s.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	if s.Name == "" {
		s.Name = s.ID[:min(8, len(s.ID))]
	}
	return s, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// List returns the conversation of every directory under root, newest first.
// One directory contributes one entry: its current session, or the newest file
// the old layout left there when the current one does not exist yet.
func List(root string) ([]Info, error) {
	return listAll(root)
}

// listAll walks every project bucket and keeps one entry per bucket: its
// current session, or the newest file from the old layout when the bucket has
// not been opened since.
func listAll(root string) ([]Info, error) {
	var infos []Info
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		// Each bucket is one directory; the session files live inside it.
		entries, rerr := os.ReadDir(path)
		if rerr != nil || len(entries) == 0 {
			return nil
		}
		// The fixed path is the directory's conversation whenever it exists;
		// otherwise the newest file the old layout left stands in for it.
		// Archived conversations are not listed: --new moved them aside.
		chosen := ""
		var chosenAt time.Time
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			p := filepath.Join(path, e.Name())
			info, serr := stat(p)
			if serr != nil || info.ID == "" {
				// Unreadable, or a leftover with no session header in it.
				continue
			}
			switch {
			case e.Name() == sessionName:
				chosen, chosenAt = p, info.UpdatedAt
			case filepath.Base(chosen) == sessionName:
				// Already holding the current conversation.
			case strings.HasPrefix(e.Name(), archivedStart):
				// Moved aside by --new; kept on disk, not listed.
			case chosen == "" || info.UpdatedAt.After(chosenAt):
				chosen, chosenAt = p, info.UpdatedAt
			}
		}
		if chosen == "" {
			return nil
		}
		if info, serr := stat(chosen); serr == nil {
			infos = append(infos, info)
		}
		return nil
	})
	sortInfos(infos)
	return infos, nil
}

// sortInfos orders sessions newest first, with a deterministic tie-break when
// two timestamps collide.
func sortInfos(infos []Info) {
	sort.Slice(infos, func(i, j int) bool {
		if !infos[i].UpdatedAt.Equal(infos[j].UpdatedAt) {
			return infos[i].UpdatedAt.After(infos[j].UpdatedAt)
		}
		return infos[i].Path > infos[j].Path
	})
}

func stat(path string) (Info, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Info{}, err
	}
	info := Info{Path: path, UpdatedAt: fi.ModTime()}
	s, err := Load(path)
	if err != nil || !s.recorded {
		return info, nil
	}
	info.ID = s.ID
	info.Name = s.Name
	info.Cwd = s.Cwd
	info.Model = s.Model
	info.Messages = len(s.Messages)
	info.Compacted = s.Compaction != nil
	if !s.UpdatedAt.IsZero() {
		info.UpdatedAt = s.UpdatedAt
	}
	return info, nil
}
