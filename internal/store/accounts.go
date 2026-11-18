package store

import (
	"database/sql"
	"errors"
	"time"
)

// Account is one WhatsApp account that can be used through MCP. Name is its
// stable identifier; JID is empty until a device has been paired.
type Account struct {
	Name      string
	JID       string
	CreatedAt time.Time
}

// PutAccount creates the account, or replaces its token if it already exists.
func (s *Store) PutAccount(name, tokenHash string) error {
	_, err := s.db.Exec(`INSERT INTO accounts (name, token_hash, created_at) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET token_hash = excluded.token_hash`,
		name, tokenHash, time.Now().Unix())
	return err
}

// SetAccountJID records (or clears, with "") the paired WhatsApp JID.
func (s *Store) SetAccountJID(name, jid string) error {
	_, err := s.db.Exec(`UPDATE accounts SET jid = ? WHERE name = ?`, jid, name)
	return err
}

// Account returns the account called name, or nil.
func (s *Store) Account(name string) (*Account, error) {
	return s.accountWhere(`name = ?`, name)
}

// AccountByTokenHash returns the account whose token hashes to tokenHash, or nil.
func (s *Store) AccountByTokenHash(tokenHash string) (*Account, error) {
	return s.accountWhere(`token_hash = ?`, tokenHash)
}

func (s *Store) accountWhere(where string, arg any) (*Account, error) {
	var a Account
	var created int64
	err := s.db.QueryRow(`SELECT name, jid, created_at FROM accounts WHERE `+where, arg).Scan(&a.Name, &a.JID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt = time.Unix(created, 0)
	return &a, nil
}

func (s *Store) Accounts() ([]Account, error) {
	rows, err := s.db.Query(`SELECT name, jid, created_at FROM accounts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		var a Account
		var created int64
		if err := rows.Scan(&a.Name, &a.JID, &created); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(created, 0)
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// DeleteAccount removes the account and everything stored for it.
func (s *Store) DeleteAccount(name string) error {
	if err := s.PurgeAccount(name); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM accounts WHERE name = ?`, name)
	return err
}
