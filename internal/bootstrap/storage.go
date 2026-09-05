package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"codexos/internal/sourcecapacity"
)

type Config struct {
	Version  int    `json:"version"`
	Enabled  bool   `json:"enabled"`
	Storage  string `json:"storage"`
	RunID    string `json:"run_id"`
	TCCAsset string `json:"tcc_asset"`
}
type JobRef struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}
type References struct {
	Enabled    bool     `json:"enabled"`
	TCCAsset   AssetRef `json:"tcc_asset"`
	Image      string   `json:"image"`
	Version    int      `json:"version"`
	RunID      string   `json:"run_id"`
	Generation uint64   `json:"generation"`
	Jobs       []JobRef `json:"jobs"`
	Limits     Limits   `json:"limits"`
}
type Manifest struct {
	RunID              string     `json:"run_id"`
	Version            int        `json:"version"`
	ID                 string     `json:"id"`
	Generation         uint64     `json:"generation"`
	Image              string     `json:"image"`
	ImageID            string     `json:"image_id"`
	TCCCommit          string     `json:"tcc_commit"`
	Request            Request    `json:"request"`
	SnapshotSHA256     string     `json:"snapshot_sha256"`
	SourceContentBytes int        `json:"source_content_bytes"`
	Inputs             []AssetRef `json:"inputs"`
	Limits             Limits     `json:"limits"`
	Started            time.Time  `json:"started"`
	Finished           time.Time  `json:"finished"`
	Result             Result     `json:"result"`
}
type diskOwner struct {
	Version int    `json:"version"`
	Run     string `json:"run"`
}
type Storage struct {
	Config    Config
	Directory string
	lock      *os.File
}

func randomID() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func readJSON(p string, v any, limit int) error {
	b, e := sourcecapacity.ReadFile(p, int64(limit))
	if e != nil {
		return e
	}
	return strictJSON(b, v, limit)
}
func syncDir(p string) error {
	d, e := os.Open(p)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func atomicJSON(p string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(p), ".write-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(append(b, '\n')); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(f.Name(), p); e != nil {
		return e
	}
	return syncDir(filepath.Dir(p))
}
func LoadConfig(run string) (*Config, error) {
	var c Config
	e := readJSON(filepath.Join(run, ConfigFilename), &c, 4096)
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if c.Version != 1 || !validID(c.RunID) || !filepath.IsAbs(c.Storage) || filepath.Clean(c.Storage) != c.Storage || !safeAssetID(c.TCCAsset) {
		return nil, errors.New("invalid bootstrap service configuration")
	}
	return &c, nil
}
func secureDirectory(p string) error {
	if e := os.MkdirAll(p, 0700); e != nil {
		return e
	}
	i, e := os.Lstat(p)
	if e != nil {
		return e
	}
	st, ok := i.Sys().(*syscall.Stat_t)
	if !i.IsDir() || i.Mode().Perm()&0077 != 0 || !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("bootstrap directory must be private and owned by UID %d: %s", os.Geteuid(), p)
	}
	return nil
}

// LockStorage holds the shared reservation through execution and publication.
// There is no queue. All production runs use one operator-owned storage root.
func LockStorage(c Config) (*Storage, error) {
	if e := secureDirectory(c.Storage); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(c.Storage, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("bootstrap busy: global storage reservation is held")
	}
	s := &Storage{c, filepath.Join(c.Storage, c.RunID), f}
	if e = s.recover(); e != nil {
		s.Close()
		return nil, e
	}
	return s, nil
}
func (s *Storage) Close() error {
	if s == nil || s.lock == nil {
		return nil
	}
	e := s.lock.Close()
	s.lock = nil
	return e
}
func (s *Storage) recover() error {
	entries, e := os.ReadDir(s.Config.Storage)
	if e != nil {
		return e
	}
	for _, ent := range entries {
		if ent.Name() == ".lock" {
			continue
		}
		if strings.HasPrefix(ent.Name(), ".init-") && validID(strings.TrimPrefix(ent.Name(), ".init-")) && ent.IsDir() {
			if e = os.RemoveAll(filepath.Join(s.Config.Storage, ent.Name())); e != nil {
				return e
			}
			continue
		}
		if !validID(ent.Name()) || !ent.IsDir() {
			return errors.New("unexpected bootstrap storage entry")
		}
		dir := filepath.Join(s.Config.Storage, ent.Name())
		var owner diskOwner
		if e = readJSON(filepath.Join(dir, "owner.json"), &owner, 4096); e != nil {
			return e
		}
		c, e := LoadConfig(owner.Run)
		if e != nil {
			return e
		}
		if c == nil { // A crash before the run/config commit leaves unreachable storage.
			if e = os.RemoveAll(dir); e != nil {
				return e
			}
			continue
		}
		if owner.Version != 1 || c.Storage != s.Config.Storage || c.RunID != ent.Name() {
			return errors.New("bootstrap store ownership mismatch")
		}
		for _, metadata := range []string{dir, filepath.Join(dir, "failures")} {
			files, err := os.ReadDir(metadata)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			for _, file := range files {
				if strings.HasPrefix(file.Name(), ".write-") && file.Type().IsRegular() {
					if e = os.Remove(filepath.Join(metadata, file.Name())); e != nil {
						return e
					}
				}
			}
		}
		jobs := filepath.Join(dir, "jobs")
		es, e := os.ReadDir(jobs)
		if e != nil {
			return e
		}
		for _, j := range es {
			if strings.HasPrefix(j.Name(), ".stage-") {
				if e = os.RemoveAll(filepath.Join(jobs, j.Name())); e != nil {
					return e
				}
			}
		}
	}
	return nil
}
func directoryBytes(p string) (int64, error) {
	var total int64
	count := 0
	e := filepath.WalkDir(p, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		count++
		if count > 65536 {
			return errors.New("bootstrap global file-count bound")
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("bootstrap storage contains a symlink")
		}
		if d.IsDir() {
			return nil
		}
		i, e := d.Info()
		if e != nil {
			return e
		}
		if !i.Mode().IsRegular() {
			return errors.New("bootstrap storage contains a special file")
		}
		total += i.Size()
		return nil
	})
	if errors.Is(e, os.ErrNotExist) {
		return 0, nil
	}
	return total, e
}
func (s *Storage) Reserve(runBytes, globalBytes int64) error {
	n, e := directoryBytes(s.Directory)
	if e != nil {
		return e
	}
	if n+runBytes > MaxRunBytes {
		return errors.New("bootstrap destination/run artifact quota exceeded")
	}
	n, e = directoryBytes(s.Config.Storage)
	if e != nil {
		return e
	}
	if n+globalBytes > MaxGlobalBytes {
		return errors.New("bootstrap aggregate storage quota exceeded")
	}
	return nil
}
func initializeStorage(s *Storage, run string) error {
	if len(mustJSON(diskOwner{1, run}))+1 > 4096 {
		return errors.New("bootstrap owner path exceeds metadata bound")
	}
	// Never expose a final store directory without complete ownership metadata.
	// A crash before rename leaves only a recognizable unpublished initializer.
	stage := filepath.Join(s.Config.Storage, ".init-"+s.Config.RunID)
	if e := os.Mkdir(stage, 0700); e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	if e := atomicJSON(filepath.Join(stage, "owner.json"), diskOwner{1, run}); e != nil {
		return e
	}
	if e := os.Mkdir(filepath.Join(stage, "jobs"), 0700); e != nil {
		return e
	}
	if e := syncDir(stage); e != nil {
		return e
	}
	if _, e := os.Lstat(s.Directory); !errors.Is(e, os.ErrNotExist) {
		return errors.New("bootstrap store ID collision")
	}
	if e := os.Rename(stage, s.Directory); e != nil {
		return e
	}
	return syncDir(s.Config.Storage)
}

// Provision is called only by the validated inactive gate. It does not change
// the feature ledger. The production operator always supplies StorageDirectory.
func Provision(run, root, asset string) error {
	if !safeAssetID(asset) {
		return errors.New("invalid pinned TCC asset ID")
	}
	c, e := LoadConfig(run)
	if e != nil {
		return e
	}
	if c != nil {
		if c.Storage != root || c.TCCAsset != asset {
			return errors.New("bootstrap pin/store changes require separate migration")
		}
		c.Enabled = true
		return atomicJSON(filepath.Join(run, ConfigFilename), c)
	}
	cfg := Config{1, true, root, randomID(), asset}
	s, e := LockStorage(cfg)
	if e != nil {
		return e
	}
	defer s.Close()
	if e = s.Reserve(4096, 4096); e != nil {
		return e
	}
	if e = initializeStorage(s, run); e != nil {
		return e
	}
	if e = atomicJSON(filepath.Join(run, ConfigFilename), cfg); e != nil {
		// A directory-sync error may follow a successful config rename. Leave
		// the complete store intact; recovery checks the actual config commit.
		return e
	}
	return syncDir(root)
}
func ReadReferences(directory string) (*References, error) {
	return readReferencesFile(filepath.Join(directory, ReferencesFilename))
}
func readReferencesFile(path string) (*References, error) {
	var r References
	e := readJSON(path, &r, MaxManifest)
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if r.Version != 1 || r.Image != Image || !safeAssetID(r.TCCAsset.ID) || r.TCCAsset.SHA256 != TCCSHA256 || !validID(r.RunID) || r.Limits != Baseline() || len(r.Jobs) > 64 {
		return nil, errors.New("invalid bootstrap artifact references")
	}
	seen := map[string]bool{}
	for _, j := range r.Jobs {
		if !validID(j.ID) || !validID(j.SHA256) || seen[j.ID] {
			return nil, errors.New("invalid bootstrap job reference")
		}
		seen[j.ID] = true
	}
	return &r, nil
}
func (s *Storage) manifest(ref JobRef) (Manifest, error) {
	var m Manifest
	if !validID(ref.ID) || !validID(ref.SHA256) {
		return m, errors.New("invalid bootstrap job reference")
	}
	b, e := sourcecapacity.ReadFile(filepath.Join(s.Directory, "jobs", ref.ID, "manifest.json"), MaxManifest)
	if e != nil {
		return m, e
	}
	if Digest(b) != ref.SHA256 {
		return m, errors.New("bootstrap manifest digest mismatch")
	}
	if e = strictJSON(b, &m, MaxManifest); e != nil {
		return m, e
	}
	if m.Version != 1 || !validID(m.RunID) || m.ID != ref.ID || m.Image != Image || m.ImageID != ImageID || m.TCCCommit != TCCCommit || m.Limits != Baseline() || m.Result.Status != 0 || !m.Result.Cleaned || !validID(m.SnapshotSHA256) || m.Finished.Before(m.Started) {
		return m, errors.New("invalid bootstrap job provenance")
	}
	if e = m.Request.Validate(); e != nil {
		return m, e
	}
	if e = sourcecapacity.Budget(m.SourceContentBytes).Validate(); e != nil {
		return m, e
	}
	if len(m.Result.Artifacts) != len(m.Request.Outputs) {
		return m, errors.New("bootstrap output manifest mismatch")
	}
	seen := map[string]bool{}
	var total int64
	for i, a := range m.Result.Artifacts {
		if !validID(a.ID) || a.Name != m.Request.Outputs[i] || seen[a.Name] || a.Size < 0 || a.Size > MaxOutput {
			return m, errors.New("invalid bootstrap artifact")
		}
		seen[a.Name] = true
		total += a.Size
	}
	if total > MaxOutputs {
		return m, errors.New("bootstrap output total exceeded")
	}
	return m, nil
}
func (s *Storage) Validate(refs *References) error {
	if refs == nil {
		return nil
	}
	if refs.Version != 1 || refs.Limits != Baseline() || len(refs.Jobs) > 64 || refs.RunID != s.Config.RunID {
		return errors.New("bootstrap references belong to another run")
	}
	var count int
	for _, ref := range refs.Jobs {
		m, e := s.manifest(ref)
		if e != nil {
			return e
		}
		count += len(m.Result.Artifacts)
		for _, a := range m.Result.Artifacts {
			b, e := sourcecapacity.ReadFile(filepath.Join(s.Directory, "jobs", ref.ID, a.ID), MaxOutput)
			if e != nil {
				return e
			}
			if int64(len(b)) != a.Size || Digest(b) != a.ID {
				return errors.New("bootstrap artifact content mismatch")
			}
		}
	}
	if count > 256 {
		return errors.New("bootstrap artifact count quota exceeded")
	}
	return s.Reserve(0, 0)
}

// ValidateArchive is read-only: inspection never repairs archives or storage.
func ValidateArchive(run, archive string) error {
	refs, e := ReadReferences(archive)
	if e != nil || refs == nil {
		return e
	}
	c, e := LoadConfig(run)
	if e != nil {
		return e
	}
	var generation uint64
	if _, e = fmt.Sscanf(filepath.Base(archive), "generation-%d", &generation); e == nil && refs.Generation != generation {
		return errors.New("bootstrap archive generation mismatch")
	}
	if c == nil {
		return errors.New("archive has bootstrap references but run configuration is missing")
	}
	s := &Storage{Config: *c, Directory: filepath.Join(c.Storage, c.RunID)}
	return s.Validate(refs)
}
func (s *Storage) Read(refs References, id string, offset, length uint64) ([]byte, error) {
	if refs.RunID != s.Config.RunID || !validID(id) || length > MaxRead || offset+length < offset {
		return nil, errors.New("invalid artifact range")
	}
	for _, ref := range refs.Jobs {
		m, e := s.manifest(ref)
		if e != nil {
			return nil, e
		}
		for _, a := range m.Result.Artifacts {
			if a.ID != id {
				continue
			}
			if offset > uint64(a.Size) || length > uint64(a.Size)-offset {
				return nil, errors.New("artifact range exceeds content")
			}
			b, e := sourcecapacity.ReadFile(filepath.Join(s.Directory, "jobs", ref.ID, id), MaxOutput)
			if e != nil {
				return nil, e
			}
			if Digest(b) != id || int64(len(b)) != a.Size {
				return nil, errors.New("artifact digest mismatch")
			}
			return append([]byte(nil), b[offset:offset+length]...), nil
		}
	}
	return nil, errors.New("artifact is not authorized for this generation lineage")
}
func (s *Storage) artifact(refs References, id string) ([]byte, error) {
	for _, r := range refs.Jobs {
		m, e := s.manifest(r)
		if e != nil {
			return nil, e
		}
		for _, a := range m.Result.Artifacts {
			if a.ID == id {
				b, e := sourcecapacity.ReadFile(filepath.Join(s.Directory, "jobs", r.ID, id), MaxOutput)
				if e != nil {
					return nil, e
				}
				if Digest(b) != id || int64(len(b)) != a.Size {
					return nil, errors.New("artifact digest mismatch")
				}
				return b, nil
			}
		}
	}
	return nil, errors.New("artifact is not authorized for this generation lineage")
}
func (s *Storage) Publish(ctx context.Context, m Manifest, outputs []Input, refs *References) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID(m.ID) || !validID(m.RunID) || m.Version != 1 || m.Result.Status != 0 || !m.Result.Cleaned || m.Limits != Baseline() || refs.RunID != s.Config.RunID {
		return errors.New("invalid or unclean bootstrap publication")
	}
	if e := m.Request.Validate(); e != nil {
		return e
	}
	if len(outputs) != len(m.Request.Outputs) || len(outputs) != len(m.Result.Artifacts) {
		return errors.New("output count mismatch")
	}
	total := 0
	for i, o := range outputs {
		total += len(o.Data)
		if o.Path != m.Request.Outputs[i] || len(o.Data) > MaxOutput {
			return errors.New("output declaration/size mismatch")
		}
	}
	if total > MaxOutputs {
		return errors.New("output total quota exceeded")
	}
	if len(mustJSON(m)) > MaxManifest {
		return errors.New("job manifest exceeds bound")
	}
	if err := s.Admit(len(outputs)); err != nil {
		return err
	}
	if len(refs.Jobs) >= 64 {
		return errors.New("bootstrap successful job quota exhausted")
	}
	count := len(outputs)
	for _, j := range refs.Jobs {
		old, e := s.manifest(j)
		if e != nil {
			return e
		}
		count += len(old.Result.Artifacts)
	}
	if count > 256 {
		return errors.New("bootstrap artifact count quota exhausted")
	}
	stage, e := os.MkdirTemp(filepath.Join(s.Directory, "jobs"), ".stage-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	for i, o := range outputs {
		if i >= len(m.Result.Artifacts) || o.Path != m.Result.Artifacts[i].Name || Digest(o.Data) != m.Result.Artifacts[i].ID || int64(len(o.Data)) != m.Result.Artifacts[i].Size {
			return errors.New("worker output identity mismatch")
		}
		p := filepath.Join(stage, Digest(o.Data))
		if _, e := os.Stat(p); e == nil {
			continue
		}
		f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
		if e != nil {
			return e
		}
		_, e = f.Write(o.Data)
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
	}
	if len(outputs) != len(m.Result.Artifacts) {
		return errors.New("missing worker outputs")
	}
	if e = atomicJSON(filepath.Join(stage, "manifest.json"), m); e != nil {
		return e
	}
	if e = syncDir(stage); e != nil {
		return e
	}
	if e = s.Reserve(0, 0); e != nil {
		return e
	}
	final := filepath.Join(s.Directory, "jobs", m.ID)
	if _, e = os.Lstat(final); !errors.Is(e, os.ErrNotExist) {
		return errors.New("bootstrap job ID collision")
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = os.Rename(stage, final); e != nil {
		return e
	}
	if e = syncDir(filepath.Dir(final)); e != nil {
		return e
	}
	b, e := sourcecapacity.ReadFile(filepath.Join(final, "manifest.json"), MaxManifest)
	if e != nil {
		return e
	}
	next := *refs
	next.Jobs = append(append([]JobRef(nil), refs.Jobs...), JobRef{m.ID, Digest(b)})
	newIndexBytes := len(mustJSON(next)) + 1
	oldIndexBytes := 0
	if info, err := os.Stat(filepath.Join(s.Directory, ReferencesFilename)); err == nil {
		oldIndexBytes = int(info.Size())
	}
	delta := int64(max(0, newIndexBytes-oldIndexBytes))
	if e = s.Reserve(delta, delta); e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	// This synced index is the commit point; an interrupted earlier rename leaves
	// an unreferenced immutable job, never a partially authorized artifact.
	if e = atomicJSON(filepath.Join(s.Directory, ReferencesFilename), next); e != nil {
		return e
	}
	*refs = next
	return nil
}
func (s *Storage) Failure(m Manifest) error {
	dir := filepath.Join(s.Directory, "failures")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	// JSON escaping can expand diagnostics sixfold; the retained failure record
	// gets a separate 64 KiB bound, keeping 32 records within 2 MiB.
	if len(m.Result.Diagnostics) > 8192 {
		m.Result.Diagnostics = m.Result.Diagnostics[:8192]
	}
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	if len(b) > MaxDiagnostics {
		return errors.New("failure record exceeds bound")
	}
	entries, e := os.ReadDir(dir)
	if e != nil {
		return e
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for len(entries) >= 32 {
		if e = os.Remove(filepath.Join(dir, entries[0].Name())); e != nil {
			return e
		}
		entries = entries[1:]
	}
	if e = s.Reserve(int64(len(b)), int64(len(b))); e != nil {
		return e
	}
	return atomicJSON(filepath.Join(dir, fmt.Sprintf("%020d-%s.json", time.Now().UnixNano(), m.ID)), m)
}
func Freeze(run, staging string, generation uint64) error {
	c, e := LoadConfig(run)
	if e != nil || c == nil {
		return e
	}
	refs, e := ReadReferences(filepath.Join(c.Storage, c.RunID))
	if e != nil {
		return e
	}
	if refs == nil {
		value := NewReferences(*c, generation)
		refs = &value
	}
	if refs.Generation != generation {
		return errors.New("bootstrap generation reference mismatch")
	}
	if e = (&Storage{Config: *c, Directory: filepath.Join(c.Storage, c.RunID)}).Validate(refs); e != nil {
		return e
	}
	return atomicJSON(filepath.Join(staging, ReferencesFilename), refs)
}

// Inherit prepares a private copy of the selected archive's jobs. The caller
// holds the returned reservation until its fresh destination directory commit.
func Inherit(source, archive, candidate, destination string) (func(bool) error, error) {
	refs, e := ReadReferences(archive)
	if e != nil {
		return nil, e
	}
	if refs == nil {
		return func(bool) error { return nil }, nil
	}
	c, e := LoadConfig(source)
	if e != nil {
		return nil, e
	}
	if c == nil {
		return nil, errors.New("missing source bootstrap store")
	}
	s, e := LockStorage(*c)
	if e != nil {
		return nil, e
	}
	fail := func(e error) (func(bool) error, error) { s.Close(); return nil, e }
	if e = s.Validate(refs); e != nil {
		return fail(e)
	}
	dest := *c
	dest.RunID = randomID()
	dest.Enabled = false
	d := &Storage{Config: dest, Directory: filepath.Join(dest.Storage, dest.RunID)}
	// Account for destination metadata as well as copied opaque content.
	var need int64 = int64(4096 + len(mustJSON(*refs)) + 1024)
	for _, ref := range refs.Jobs {
		n, e := directoryBytes(filepath.Join(s.Directory, "jobs", ref.ID))
		if e != nil {
			return fail(e)
		}
		need += n
	}
	if e = d.Reserve(need, need); e != nil {
		return fail(e)
	}
	if e = initializeStorage(d, destination); e != nil {
		return fail(e)
	}
	done := func(ok bool) error {
		var e error
		if !ok {
			e = os.RemoveAll(d.Directory)
		}
		return errors.Join(e, s.Close())
	}
	for _, ref := range refs.Jobs {
		m, e := s.manifest(ref)
		if e != nil {
			done(false)
			return nil, e
		}
		target := filepath.Join(d.Directory, "jobs", ref.ID)
		if e = os.Mkdir(target, 0700); e != nil {
			done(false)
			return nil, e
		}
		names := map[string]bool{"manifest.json": true}
		for _, a := range m.Result.Artifacts {
			names[a.ID] = true
		}
		for name := range names {
			data, e := sourcecapacity.ReadFile(filepath.Join(s.Directory, "jobs", ref.ID, name), MaxOutput)
			if e == nil {
				e = os.WriteFile(filepath.Join(target, name), data, 0400)
			}
			if e != nil {
				done(false)
				return nil, e
			}
			f, e := os.Open(filepath.Join(target, name))
			if e != nil {
				done(false)
				return nil, e
			}
			e = f.Sync()
			f.Close()
			if e != nil {
				done(false)
				return nil, e
			}
		}
		if e = syncDir(target); e != nil {
			done(false)
			return nil, e
		}
	}
	next := *refs
	next.RunID = dest.RunID
	next.Enabled = false
	next.Generation = 0
	if e = atomicJSON(filepath.Join(d.Directory, ReferencesFilename), next); e == nil {
		e = atomicJSON(filepath.Join(candidate, ConfigFilename), dest)
	}
	if e == nil {
		e = atomicJSON(filepath.Join(candidate, "bootstrap-inherited.json"), next)
	}
	if e == nil {
		e = syncDir(filepath.Join(d.Directory, "jobs"))
	}
	if e == nil {
		e = syncDir(dest.Storage)
	}
	if e != nil {
		done(false)
		return nil, e
	}
	return done, nil
}

// copyBounded reads regular files only; collection from hostile container paths
// has the stronger openat2/process-identity checks in collector_linux.go.
func copyBounded(dst io.Writer, src io.Reader, n int64) error {
	written, e := io.Copy(dst, io.LimitReader(src, n+1))
	if e != nil {
		return e
	}
	if written > n {
		return errors.New("byte limit exceeded")
	}
	return nil
}

// Admit counts all retained successes; rollback does not reset object quotas.
func (s *Storage) Admit(outputs int) error {
	entries, err := os.ReadDir(filepath.Join(s.Directory, "jobs"))
	if err != nil {
		return err
	}
	jobs, artifacts := 0, outputs
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".stage-") {
			continue
		}
		if !validID(entry.Name()) || !entry.IsDir() {
			return errors.New("invalid retained bootstrap job")
		}
		b, err := sourcecapacity.ReadFile(filepath.Join(s.Directory, "jobs", entry.Name(), "manifest.json"), MaxManifest)
		if err != nil {
			return err
		}
		m, err := s.manifest(JobRef{entry.Name(), Digest(b)})
		if err != nil {
			return err
		}
		jobs++
		artifacts += len(m.Result.Artifacts)
	}
	if jobs >= 64 || artifacts > 256 {
		return errors.New("bootstrap retained job/artifact quota exhausted")
	}
	return nil
}

// GarbageCollect removes only successes absent from every immutable archive and
// imported parent. The caller owns a validated inactive gate; no live cursor is
// interpreted as permission to delete an archived compiler or sysroot.
func GarbageCollect(run string) (int, error) {
	c, e := LoadConfig(run)
	if e != nil || c == nil {
		return 0, e
	}
	s, e := LockStorage(*c)
	if e != nil {
		return 0, e
	}
	defer s.Close()
	keep := map[string]bool{}
	entries, e := os.ReadDir(run)
	if e != nil {
		return 0, e
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "generation-") {
			continue
		}
		refs, e := ReadReferences(filepath.Join(run, entry.Name()))
		if e != nil {
			return 0, e
		}
		if e = s.Validate(refs); e != nil {
			return 0, e
		}
		if refs != nil {
			for _, j := range refs.Jobs {
				keep[j.ID] = true
			}
		}
	}
	var inherited References
	e = readJSON(filepath.Join(run, "bootstrap-inherited.json"), &inherited, MaxManifest)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return 0, e
	}
	if e == nil {
		for _, j := range inherited.Jobs {
			keep[j.ID] = true
		}
	}
	current, e := ReadReferences(s.Directory)
	if e != nil {
		return 0, e
	}
	if current != nil {
		filtered := make([]JobRef, 0, len(current.Jobs))
		for _, j := range current.Jobs {
			if keep[j.ID] {
				filtered = append(filtered, j)
			}
		}
		current.Jobs = filtered
		if e = atomicJSON(filepath.Join(s.Directory, ReferencesFilename), current); e != nil {
			return 0, e
		}
	}
	jobs, e := os.ReadDir(filepath.Join(s.Directory, "jobs"))
	if e != nil {
		return 0, e
	}
	count := 0
	for _, job := range jobs {
		if !validID(job.Name()) {
			return count, errors.New("unexpected job storage entry")
		}
		if !keep[job.Name()] {
			if e = os.RemoveAll(filepath.Join(s.Directory, "jobs", job.Name())); e != nil {
				return count, e
			}
			count++
		}
	}
	return count, syncDir(filepath.Join(s.Directory, "jobs"))
}

func NewReferences(c Config, generation uint64) References {
	return References{Version: 1, Enabled: c.Enabled, TCCAsset: AssetRef{c.TCCAsset, TCCSHA256}, Image: Image, RunID: c.RunID, Generation: generation, Limits: Baseline()}
}

// ProvisionInherited is called only after the runtime validates an unstarted
// cross-run destination and its frozen TCC asset. It authorizes existing copied
// bytes, never creates a fresh store or inherits an execution grant.
func ProvisionInherited(ctx context.Context, run, asset string, client *Client) error {
	c, e := LoadConfig(run)
	if e != nil {
		return e
	}
	if c == nil || c.TCCAsset != asset {
		return errors.New("initial bootstrap provisioning requires matching inherited artifacts")
	}
	refs, e := readReferencesFile(filepath.Join(run, "bootstrap-inherited.json"))
	if e != nil {
		return e
	}
	if refs == nil || refs.Generation != 0 || refs.Enabled || refs.TCCAsset.ID != asset {
		return errors.New("initial bootstrap inheritance references are missing or invalid")
	}
	s, e := LockStorage(*c)
	if e != nil {
		return e
	}
	defer s.Close()
	if e = s.Validate(refs); e != nil {
		return e
	}
	if e = client.Probe(ctx); e != nil {
		return e
	}
	c.Enabled = true
	return atomicJSON(filepath.Join(run, ConfigFilename), c)
}
