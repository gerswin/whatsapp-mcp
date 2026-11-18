package wa

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// ParseRecipient turns what a person would type (a phone number, with or
// without "+", spaces or dashes) or a WhatsApp JID into a JID.
func ParseRecipient(to string) (types.JID, error) {
	to = strings.TrimSpace(to)
	if to == "" {
		return types.JID{}, errors.New("recipient is required")
	}

	if strings.Contains(to, "@") {
		jid, err := types.ParseJID(to)
		if err != nil || jid.User == "" {
			return types.JID{}, errors.New("invalid recipient: use a phone number with country code or a WhatsApp JID")
		}
		return jid.ToNonAD(), nil
	}

	var digits strings.Builder
	for _, r := range to {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '+' || r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
		default:
			return types.JID{}, errors.New("invalid recipient: use a phone number with country code or a WhatsApp JID")
		}
	}
	if digits.Len() < 7 || digits.Len() > 15 {
		return types.JID{}, errors.New("invalid recipient: phone numbers need the country code and 7 to 15 digits")
	}
	return types.NewJID(digits.String(), types.DefaultUserServer), nil
}

type Contact struct {
	JID          string
	Name         string
	PushName     string
	BusinessName string
}

func (m *Manager) Contacts(ctx context.Context, account string) ([]Contact, error) {
	client := m.Client(account)
	if client == nil || client.Store.Contacts == nil {
		return nil, errors.New("this account is not linked to WhatsApp; it has to be paired again on the server")
	}

	all, err := client.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return nil, err
	}

	contacts := make([]Contact, 0, len(all))
	for jid, c := range all {
		// LID entries duplicate the phone-number ones and are not addressable by users.
		if jid.Server != types.DefaultUserServer {
			continue
		}
		contacts = append(contacts, Contact{
			JID:          jid.ToNonAD().String(),
			Name:         firstNonEmpty(c.FullName, c.FirstName),
			PushName:     c.PushName,
			BusinessName: c.BusinessName,
		})
	}
	return contacts, nil
}

// ContactName returns the best known name of a 1:1 chat, or "".
func (m *Manager) ContactName(ctx context.Context, account, jid string) string {
	client := m.Client(account)
	if client == nil || client.Store.Contacts == nil {
		return ""
	}
	parsed, err := types.ParseJID(jid)
	if err != nil {
		return ""
	}
	c, err := client.Store.Contacts.GetContact(ctx, parsed)
	if err != nil || !c.Found {
		return ""
	}
	return firstNonEmpty(c.FullName, c.FirstName, c.BusinessName, c.PushName)
}

type NumberCheck struct {
	Query        string
	OnWhatsApp   bool
	JID          string
	VerifiedName string
}

func (m *Manager) CheckNumbers(ctx context.Context, account string, numbers []string) ([]NumberCheck, error) {
	client, err := m.ready(account)
	if err != nil {
		return nil, err
	}

	phones := make([]string, 0, len(numbers))
	for _, n := range numbers {
		jid, err := ParseRecipient(n)
		if err != nil || jid.Server != types.DefaultUserServer {
			return nil, errors.New("numbers must be phone numbers in international format: " + n)
		}
		phones = append(phones, "+"+jid.User)
	}

	resp, err := client.IsOnWhatsApp(ctx, phones)
	if err != nil {
		return nil, err
	}

	out := make([]NumberCheck, 0, len(resp))
	for _, r := range resp {
		check := NumberCheck{Query: r.Query, OnWhatsApp: r.IsIn}
		if r.IsIn {
			check.JID = r.JID.ToNonAD().String()
		}
		if r.VerifiedName != nil && r.VerifiedName.Details != nil {
			check.VerifiedName = r.VerifiedName.Details.GetVerifiedName()
		}
		out = append(out, check)
	}
	return out, nil
}

type Group struct {
	JID          string
	Name         string
	Participants int
}

func (m *Manager) Groups(ctx context.Context, account string) ([]Group, error) {
	client, err := m.ready(account)
	if err != nil {
		return nil, err
	}

	joined, err := client.GetJoinedGroups(ctx)
	if err != nil {
		return nil, err
	}

	groups := make([]Group, 0, len(joined))
	for _, g := range joined {
		if g == nil {
			continue
		}
		groups = append(groups, Group{JID: g.JID.String(), Name: g.Name, Participants: len(g.Participants)})
		_ = m.store.SetChatName(account, g.JID.String(), g.Name)
	}
	return groups, nil
}

type GroupParticipant struct {
	JID     string
	Phone   string
	IsAdmin bool
}

type GroupDetails struct {
	JID          string
	Name         string
	Description  string
	Owner        string
	CreatedAt    time.Time
	AdminsOnly   bool
	Participants []GroupParticipant
}

func (m *Manager) GroupInfo(ctx context.Context, account, group string) (*GroupDetails, error) {
	client, err := m.ready(account)
	if err != nil {
		return nil, err
	}
	jid, err := types.ParseJID(group)
	if err != nil || jid.Server != types.GroupServer {
		return nil, errors.New("group must be a group JID ending in @g.us (see list_groups)")
	}

	g, err := client.GetGroupInfo(ctx, jid)
	if err != nil {
		return nil, err
	}

	out := &GroupDetails{
		JID:          g.JID.String(),
		Name:         g.Name,
		Description:  g.Topic,
		CreatedAt:    g.GroupCreated,
		AdminsOnly:   g.IsAnnounce,
		Participants: make([]GroupParticipant, 0, len(g.Participants)),
	}
	for _, owner := range []types.JID{g.OwnerPN, g.OwnerJID} {
		if !owner.IsEmpty() {
			out.Owner = owner.ToNonAD().String()
			break
		}
	}
	for _, p := range g.Participants {
		gp := GroupParticipant{JID: p.JID.String(), IsAdmin: p.IsAdmin || p.IsSuperAdmin}
		if !p.PhoneNumber.IsEmpty() {
			gp.Phone = p.PhoneNumber.User
		}
		out.Participants = append(out.Participants, gp)
	}
	return out, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
