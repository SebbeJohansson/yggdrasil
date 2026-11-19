package norn

// The Obsidian notes changed in a range, for Rewind: titles and links only,
// never note text. A file's modification time says only its last change, so
// every few minutes Norn notes each note's path and modification time
// (rewind_vault), and a note changed again later still shows on the earlier
// day. That log starts when Norn first runs with it; before it, only a note's
// last change is known.

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

const (
	vaultLogSchema = `CREATE TABLE IF NOT EXISTS rewind_vault (
	path TEXT NOT NULL,
	at INTEGER NOT NULL,
	PRIMARY KEY (path, at)
);
CREATE INDEX IF NOT EXISTS rewind_vault_by_at ON rewind_vault (at);`
	// vaultLogEvery is how often the vault's notes are looked at.
	vaultLogEvery = 5 * time.Minute
	// vaultNotesMax is the most notes one walk of the vault reads.
	vaultNotesMax = 50_000
)

// NoteChange is a note seen changed at At.
type NoteChange struct {
	Path  string
	Title string
	Link  string
	At    int64
}

// VaultChanges is every note in the vault with its last change: Markdown
// files anywhere but in hidden folders (.obsidian, .trash).
func (v *Vault) VaultChanges() []NoteChange {
	out := []NoteChange{}
	_ = filepath.WalkDir(v.Dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != v.Dir && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") || len(out) >= vaultNotesMax {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(v.Dir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		out = append(out, NoteChange{Path: rel, Title: strings.TrimSuffix(name, ".md"), Link: v.link(rel), At: info.ModTime().UnixMilli()})
		return nil
	})
	return out
}

func (n *Norn) initVaultLog(ctx context.Context) error {
	n.Rewind.vaultMu.Lock()
	defer n.Rewind.vaultMu.Unlock()
	if n.Rewind.vaultInit {
		return nil
	}
	if _, err := n.Store.db.ExecContext(ctx, vaultLogSchema); err != nil {
		return err
	}
	n.Rewind.vaultInit = true
	return nil
}

// logVault notes each note's last change; the same note and time is kept once.
func (n *Norn) logVault(ctx context.Context, now time.Time) error {
	if !n.Vault.On() {
		return nil
	}
	if err := n.initVaultLog(ctx); err != nil {
		return err
	}
	changes := n.Vault.VaultChanges()
	tx, err := n.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range changes {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO rewind_vault (path, at) VALUES (?, ?)`, c.Path, c.At); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// noteChanges is the notes changed in from..to: from the log, and the vault as it is now.
func (n *Norn) noteChanges(ctx context.Context, from, to int64, now time.Time) ([]NoteChange, error) {
	if err := n.initVaultLog(ctx); err != nil {
		return nil, err
	}
	changes := []NoteChange{}
	for _, c := range n.Vault.VaultChanges() {
		if inside(c.At, from, to) {
			changes = append(changes, c)
		}
	}
	rows, err := n.Store.db.QueryContext(ctx, `SELECT path, at FROM rewind_vault WHERE at >= ? AND at < ?`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c NoteChange
		if err := rows.Scan(&c.Path, &c.At); err != nil {
			return nil, err
		}
		c.Title = strings.TrimSuffix(filepath.Base(c.Path), ".md")
		c.Link = n.Vault.link(c.Path)
		changes = append(changes, c)
	}
	return changes, rows.Err()
}
