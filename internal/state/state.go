// Package state stores each registered owner's resolved routes, one JSON file
// per owner directory, under a lock shared by every devingress invocation.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/WonderPanda/devingress/internal/routefile"
)

type Store struct{ dir string }

func New(dir string) Store { return Store{dir: dir} }

func (s Store) ownersDir() string { return filepath.Join(s.dir, "owners") }

func key(ownerDir string) string {
	sum := sha256.Sum256([]byte(ownerDir))
	return hex.EncodeToString(sum[:6])
}

// Lock serializes devingress invocations: worktree hooks run concurrently, and two
// interleaved reconciles would each drop the other's owner.
func (s Store) Lock() (unlock func(), err error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// List returns every registered owner, sorted by directory.
func (s Store) List() ([]routefile.Owner, error) {
	entries, err := os.ReadDir(s.ownersDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []routefile.Owner
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.ownersDir(), e.Name()))
		if err != nil {
			return nil, err
		}
		var o routefile.Owner
		if err := json.Unmarshal(data, &o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

func (s Store) Save(o routefile.Owner) error {
	if err := os.MkdirAll(s.ownersDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.ownersDir(), key(o.Dir)+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s Store) Remove(ownerDir string) error {
	err := os.Remove(filepath.Join(s.ownersDir(), key(ownerDir)+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
