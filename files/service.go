package files

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
)

// Service is delivered files over their records and their bytes.
type Service struct {
	Store Store
	Blobs Blobs
	Now   func() time.Time // time.Now when nil
	// Log records each read, download and delete by the file's id and who
	// asked, never its name or what it holds; nil logs nothing.
	Log *slog.Logger
	// MaxFiles and MaxBytes are an account's total; 0: MaxAccountN and
	// MaxAccountSize.
	MaxFiles int
	MaxBytes int64
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log(msg string, args ...any) {
	if s.Log != nil {
		s.Log.Info(msg, args...)
	}
}

// Delivery is what a run delivers.
type Delivery struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Content   string `json:"content"`
}

// Deliver keeps a file run of tenant's agent made, in chat session: checked,
// cleaned and stored for Kept. A run delivers at most MaxPerRun. When the
// account would pass MaxAccountN files or MaxAccountSize bytes, its oldest
// files go first, so a delivery is never refused for the account's total.
func (s *Service) Deliver(ctx context.Context, tenant, agent, run, session string, d Delivery) (File, error) {
	if tenant == "" || agent == "" || run == "" {
		return File{}, fmt.Errorf("%w: a file is delivered by a taf's run", ErrInvalid)
	}
	data, err := Content(d.MediaType, []byte(d.Content))
	if err != nil {
		return File{}, err
	}
	all, err := s.Store.List(ctx, tenant)
	if err != nil {
		return File{}, err
	}
	now := s.now()
	all = slices.DeleteFunc(all, func(f File) bool { return !f.Expires.After(now) })
	if n := len(slices.DeleteFunc(slices.Clone(all), func(f File) bool { return f.Run != run })); n >= MaxPerRun {
		return File{}, fmt.Errorf("%w: a run delivers at most %d files, and this one has", ErrInvalid, MaxPerRun)
	}
	sum := sha256.Sum256(data)
	f := File{ID: newID(), Tenant: tenant, Agent: agent, Run: run, Session: session, Name: Name(d.Name, d.MediaType),
		MediaType: d.MediaType, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Created: now, Expires: now.Add(Kept)}
	if err := s.makeRoom(ctx, all, f.Size); err != nil {
		return File{}, err
	}
	if err := s.Blobs.Put(ctx, f.Key(), Types[f.MediaType].Served, data); err != nil {
		return File{}, err
	}
	if err := s.Store.Create(ctx, f); err != nil {
		_ = s.Blobs.Delete(ctx, f.Key())
		return File{}, err
	}
	s.log("file delivered", "tenant", tenant, "taf", agent, "run", run, "file", f.ID, "size", f.Size)
	return f, nil
}

// makeRoom removes the account's oldest files until one more of size fits
// under MaxAccountN and MaxAccountSize.
func (s *Service) makeRoom(ctx context.Context, all []File, size int64) error {
	maxN, maxSize := MaxAccountN, int64(MaxAccountSize)
	if s.MaxFiles > 0 {
		maxN = s.MaxFiles
	}
	if s.MaxBytes > 0 {
		maxSize = s.MaxBytes
	}
	slices.SortFunc(all, func(a, b File) int { return a.Created.Compare(b.Created) })
	var total int64
	for _, f := range all {
		total += f.Size
	}
	for len(all) > 0 && (len(all)+1 > maxN || total+size > maxSize) {
		old := all[0]
		if err := s.remove(ctx, old); err != nil {
			return err
		}
		s.log("file removed for room", "tenant", old.Tenant, "file", old.ID, "size", old.Size)
		total -= old.Size
		all = all[1:]
	}
	return nil
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "fil_" + hex.EncodeToString(b)
}

// List is the tenant's files that filter keeps, newest first, none past its
// Expires.
func (s *Service) List(ctx context.Context, tenant string, filter Filter) ([]File, error) {
	all, err := s.Store.List(ctx, tenant)
	if err != nil {
		return nil, err
	}
	now := s.now()
	all = slices.DeleteFunc(all, func(f File) bool { return !filter.keeps(f) || !f.Expires.After(now) })
	slices.SortFunc(all, func(a, b File) int {
		if c := b.Created.Compare(a.Created); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	return all, nil
}

// Get is one of the tenant's files: ErrNotFound for another account's id, or
// one past its Expires.
func (s *Service) Get(ctx context.Context, tenant, id string) (File, error) {
	f, err := s.Store.Get(ctx, tenant, id)
	if err != nil {
		return File{}, err
	}
	if f.Tenant != tenant || !f.Expires.After(s.now()) {
		return File{}, ErrNotFound
	}
	return f, nil
}

// Read is one file and its preview (Preview), for member, who's on the
// tenant's account (the caller checks that).
func (s *Service) Read(ctx context.Context, tenant, id, member string) (File, string, error) {
	f, err := s.Get(ctx, tenant, id)
	if err != nil {
		return File{}, "", err
	}
	data, err := s.Blobs.Get(ctx, f.Key(), f.Size)
	if err != nil {
		return File{}, "", err
	}
	s.log("file read", "tenant", tenant, "file", id, "by", member)
	return f, Preview(data), nil
}

// Download is a URL to one file's bytes for ReadTTL, served as an attachment
// of its checked type under its cleaned name.
func (s *Service) Download(ctx context.Context, tenant, id, member string) (File, string, error) {
	f, err := s.Get(ctx, tenant, id)
	if err != nil {
		return File{}, "", err
	}
	url, err := s.Blobs.PresignGetAs(ctx, f.Key(), f.Name, Types[f.MediaType].Served, ReadTTL)
	if err != nil {
		return File{}, "", err
	}
	s.log("file downloaded", "tenant", tenant, "file", id, "by", member)
	return f, url, nil
}

// Delete removes one file, its bytes and its record, for everyone on the
// account, and logs who did.
func (s *Service) Delete(ctx context.Context, tenant, id, member string) error {
	f, err := s.Get(ctx, tenant, id)
	if err != nil {
		return err
	}
	if err := s.remove(ctx, f); err != nil {
		return err
	}
	s.log("file deleted", "tenant", tenant, "file", id, "by", member)
	return nil
}

// RemoveAgent removes a taf's files, records and bytes.
func (s *Service) RemoveAgent(ctx context.Context, tenant, agent string) error {
	all, err := s.Store.List(ctx, tenant)
	if err != nil {
		return err
	}
	for _, f := range all {
		if f.Agent == agent {
			if err := s.Store.Delete(ctx, tenant, f.ID); err != nil {
				return err
			}
		}
	}
	return s.Blobs.DeletePrefix(ctx, AgentPrefix(tenant, agent))
}

// EraseTenant removes every file the tenant's tafs delivered, records and
// bytes.
func (s *Service) EraseTenant(ctx context.Context, tenant string) error {
	all, err := s.Store.List(ctx, tenant)
	if err != nil {
		return err
	}
	for _, f := range all {
		if err := s.Store.Delete(ctx, tenant, f.ID); err != nil {
			return err
		}
	}
	return s.Blobs.DeletePrefix(ctx, AgentPrefix(tenant, ""))
}

// Sweep removes files past their Expires, and returns how many.
func (s *Service) Sweep(ctx context.Context, limit int) (int, error) {
	due, err := s.Store.Due(ctx, s.now(), limit)
	if err != nil {
		return 0, err
	}
	for i, f := range due {
		if err := s.remove(ctx, f); err != nil {
			return i, err
		}
	}
	return len(due), nil
}

// remove deletes f's bytes, then its record: a removal cut short is finished
// by the sweep, since the record is still due.
func (s *Service) remove(ctx context.Context, f File) error {
	if err := s.Blobs.Delete(ctx, f.Key()); err != nil {
		return err
	}
	return s.Store.Delete(ctx, f.Tenant, f.ID)
}
