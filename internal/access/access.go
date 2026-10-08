// Package access decides who may use the LLM service, in which role and with
// which quota tier.
//
// The inputs are the role-provider tokens of a person ("user:a@dhbw.de",
// "group:wwi23seb", "group:dhbw-ma#dozent") and an access list that maps such
// tokens to a role and a tier. Nothing here talks to the network: callers pass
// the tokens in, which keeps the decision a pure function and testable.
package access

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Role is what a person may do. Ordered: a higher role includes the lower ones.
type Role string

const (
	RoleNone       Role = ""
	RoleUser       Role = "user"
	RoleFleetAdmin Role = "fleet-admin"
	RoleAdmin      Role = "admin"
)

// rank orders the roles. A person matching several rules gets the highest.
func (r Role) rank() int {
	switch r {
	case RoleUser:
		return 1
	case RoleFleetAdmin:
		return 2
	case RoleAdmin:
		return 3
	default:
		return 0
	}
}

// AtLeast reports whether r includes the rights of min.
func (r Role) AtLeast(min Role) bool { return r.rank() >= min.rank() && r != RoleNone }

// ParseRole accepts exactly the three role names.
func ParseRole(s string) (Role, error) {
	switch r := Role(strings.TrimSpace(s)); r {
	case RoleUser, RoleFleetAdmin, RoleAdmin:
		return r, nil
	default:
		return RoleNone, fmt.Errorf("unknown role %q (want user, fleet-admin or admin)", s)
	}
}

// Rule maps one role-provider token to a role and a quota tier.
type Rule struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	Token     string    `json:"token" gorm:"uniqueIndex;not null"`
	Role      Role      `json:"role" gorm:"not null"`
	Tier      string    `json:"tier" gorm:"not null"`
	Comment   string    `json:"comment"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
	// Bootstrap rules come from the configuration, not from the database. They
	// guarantee that someone can always administer the list, and they cannot be
	// edited or deleted through the API.
	Bootstrap bool `json:"bootstrap" gorm:"-"`
}

// TableName pins the table name so a rename of the type cannot move the data.
func (Rule) TableName() string { return "access_rules" }

var ErrInvalidToken = errors.New("token must look like user:<email> or group:<id>")

// NormaliseToken trims and lowercases a token and checks its shape. Group ids
// and e-mail addresses are case-insensitive in the role-provider as well.
func NormaliseToken(raw string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(raw))
	kind, rest, ok := strings.Cut(t, ":")
	if !ok || rest == "" || strings.ContainsAny(rest, " \t") {
		return "", ErrInvalidToken
	}
	switch kind {
	case "user":
		if !strings.Contains(rest, "@") {
			return "", ErrInvalidToken
		}
	case "group":
	default:
		return "", ErrInvalidToken
	}
	return t, nil
}

// Decision is the outcome for one person.
type Decision struct {
	Role Role   `json:"role"`
	Tier string `json:"tier"`
	// The rule that decided, for display and audit. Empty without access.
	MatchedToken string `json:"matched_token,omitempty"`
}

// Decide picks the rule that applies to a person with the given tokens.
//
// Highest role wins. Among rules of that role, a user: rule beats a group: rule
// (it was written for this person specifically), and remaining ties are broken
// by the token itself so the outcome never depends on database order.
func Decide(rules []Rule, tokens []string) Decision {
	held := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		held[strings.ToLower(strings.TrimSpace(t))] = struct{}{}
	}
	var matched []Rule
	for _, r := range rules {
		if _, ok := held[r.Token]; ok {
			matched = append(matched, r)
		}
	}
	if len(matched) == 0 {
		return Decision{}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		if a.Role.rank() != b.Role.rank() {
			return a.Role.rank() > b.Role.rank()
		}
		au, bu := strings.HasPrefix(a.Token, "user:"), strings.HasPrefix(b.Token, "user:")
		if au != bu {
			return au
		}
		return a.Token < b.Token
	})
	best := matched[0]
	return Decision{Role: best.Role, Tier: best.Tier, MatchedToken: best.Token}
}
