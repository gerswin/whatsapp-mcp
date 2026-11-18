package store

import (
	"encoding/json"
	"strings"
	"time"
)

// Reaction is one person's emoji on a message.
type Reaction struct {
	SenderJID string
	FromMe    bool
	Emoji     string
	Timestamp time.Time
}

// PollVote is the set of options one person currently has selected in a poll.
type PollVote struct {
	VoterJID  string
	FromMe    bool
	Options   []string
	Timestamp time.Time
}

// Reactions and votes are kept as "latest state per person". Removing a
// reaction or clearing a vote stores an empty value instead of deleting the
// row, so an older copy replayed later (history sync) cannot bring it back.
const extrasSchema = `
CREATE TABLE IF NOT EXISTS reactions (
	account    TEXT NOT NULL,
	chat_jid   TEXT NOT NULL,
	message_id TEXT NOT NULL,
	sender_jid TEXT NOT NULL,
	from_me    INTEGER NOT NULL DEFAULT 0,
	emoji      TEXT NOT NULL DEFAULT '',
	timestamp  INTEGER NOT NULL,
	PRIMARY KEY (account, chat_jid, message_id, sender_jid)
);
CREATE TABLE IF NOT EXISTS polls (
	account    TEXT NOT NULL,
	chat_jid   TEXT NOT NULL,
	message_id TEXT NOT NULL,
	options    TEXT NOT NULL,
	PRIMARY KEY (account, chat_jid, message_id)
);
CREATE TABLE IF NOT EXISTS poll_votes (
	account    TEXT NOT NULL,
	chat_jid   TEXT NOT NULL,
	message_id TEXT NOT NULL,
	voter_jid  TEXT NOT NULL,
	from_me    INTEGER NOT NULL DEFAULT 0,
	options    TEXT NOT NULL DEFAULT '[]',
	timestamp  INTEGER NOT NULL,
	PRIMARY KEY (account, chat_jid, message_id, voter_jid)
);
`

// extraTables are the per-message tables that follow a message's lifetime.
var extraTables = []string{"reactions", "polls", "poll_votes"}

// SetReaction records the emoji a person has on a message; an empty emoji
// means they removed it. Older updates than the stored one are ignored.
func (s *Store) SetReaction(account, chatJID, messageID, senderJID string, fromMe bool, emoji string, ts time.Time) error {
	if chatJID == "" || messageID == "" || senderJID == "" {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO reactions (account, chat_jid, message_id, sender_jid, from_me, emoji, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (account, chat_jid, message_id, sender_jid) DO UPDATE
		SET emoji = excluded.emoji, from_me = excluded.from_me, timestamp = excluded.timestamp
		WHERE excluded.timestamp >= reactions.timestamp`,
		account, chatJID, messageID, senderJID, fromMe, emoji, ts.UnixMilli())
	return err
}

// SetPollVote records the options a person has selected in a poll; no options
// means they withdrew their vote. Older updates than the stored one are ignored.
func (s *Store) SetPollVote(account, chatJID, messageID, voterJID string, fromMe bool, options []string, ts time.Time) error {
	if chatJID == "" || messageID == "" || voterJID == "" {
		return nil
	}
	if options == nil {
		options = []string{}
	}
	encoded, err := json.Marshal(options)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO poll_votes (account, chat_jid, message_id, voter_jid, from_me, options, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (account, chat_jid, message_id, voter_jid) DO UPDATE
		SET options = excluded.options, from_me = excluded.from_me, timestamp = excluded.timestamp
		WHERE excluded.timestamp >= poll_votes.timestamp`,
		account, chatJID, messageID, voterJID, fromMe, string(encoded), ts.UnixMilli())
	return err
}

// PollOptions returns the option names of a stored poll, in order, or nil.
func (s *Store) PollOptions(account, chatJID, messageID string) []string {
	var encoded string
	if err := s.db.QueryRow(`SELECT options FROM polls WHERE account = ? AND chat_jid = ? AND message_id = ?`,
		account, chatJID, messageID).Scan(&encoded); err != nil {
		return nil
	}
	var options []string
	_ = json.Unmarshal([]byte(encoded), &options)
	return options
}

// Annotate fills in the reactions of msgs and, for polls, their options and
// votes. msgs may span several chats.
func (s *Store) Annotate(account string, msgs []Message) error {
	byChat := map[string]map[string]*Message{}
	for i := range msgs {
		m := &msgs[i]
		if byChat[m.ChatJID] == nil {
			byChat[m.ChatJID] = map[string]*Message{}
		}
		byChat[m.ChatJID][m.ID] = m
	}

	for chat, byID := range byChat {
		args := make([]any, 0, len(byID)+2)
		args = append(args, account, chat)
		polls := false
		for id, m := range byID {
			args = append(args, id)
			polls = polls || m.Type == "poll"
		}
		where := ` WHERE account = ? AND chat_jid = ? AND message_id IN (?` + strings.Repeat(",?", len(byID)-1) + `)`

		if err := s.annotateReactions(where, args, byID); err != nil {
			return err
		}
		if polls {
			if err := s.annotatePolls(where, args, byID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) annotateReactions(where string, args []any, byID map[string]*Message) error {
	rows, err := s.db.Query(`SELECT message_id, sender_jid, from_me, emoji, timestamp FROM reactions`+where+
		` AND emoji != '' ORDER BY timestamp`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var r Reaction
		var ts int64
		if err := rows.Scan(&id, &r.SenderJID, &r.FromMe, &r.Emoji, &ts); err != nil {
			return err
		}
		r.Timestamp = time.UnixMilli(ts)
		if m := byID[id]; m != nil {
			m.Reactions = append(m.Reactions, r)
		}
	}
	return rows.Err()
}

func (s *Store) annotatePolls(where string, args []any, byID map[string]*Message) error {
	options, err := s.db.Query(`SELECT message_id, options FROM polls`+where, args...)
	if err != nil {
		return err
	}
	defer options.Close()
	for options.Next() {
		var id, encoded string
		if err := options.Scan(&id, &encoded); err != nil {
			return err
		}
		if m := byID[id]; m != nil {
			_ = json.Unmarshal([]byte(encoded), &m.PollOptions)
		}
	}
	if err := options.Err(); err != nil {
		return err
	}

	votes, err := s.db.Query(`SELECT message_id, voter_jid, from_me, options, timestamp FROM poll_votes`+where+
		` AND options != '[]' ORDER BY timestamp`, args...)
	if err != nil {
		return err
	}
	defer votes.Close()
	for votes.Next() {
		var id, encoded string
		var v PollVote
		var ts int64
		if err := votes.Scan(&id, &v.VoterJID, &v.FromMe, &encoded, &ts); err != nil {
			return err
		}
		_ = json.Unmarshal([]byte(encoded), &v.Options)
		v.Timestamp = time.UnixMilli(ts)
		if m := byID[id]; m != nil {
			m.Votes = append(m.Votes, v)
		}
	}
	return votes.Err()
}

// dropExtras removes the reactions and poll data of one message.
func (s *Store) dropExtras(account, chatJID, messageID string) error {
	for _, table := range extraTables {
		if _, err := s.db.Exec(`DELETE FROM `+table+` WHERE account = ? AND chat_jid = ? AND message_id = ?`,
			account, chatJID, messageID); err != nil {
			return err
		}
	}
	return nil
}

// dropOrphanExtras removes reactions and poll data whose message is gone.
func (s *Store) dropOrphanExtras() error {
	for _, table := range extraTables {
		if _, err := s.db.Exec(`DELETE FROM ` + table + ` WHERE NOT EXISTS (
			SELECT 1 FROM messages m
			WHERE m.account = ` + table + `.account AND m.chat_jid = ` + table + `.chat_jid AND m.message_id = ` + table + `.message_id)`); err != nil {
			return err
		}
	}
	return nil
}
