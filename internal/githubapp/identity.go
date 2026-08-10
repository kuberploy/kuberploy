package githubapp

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	loginPattern      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38}[A-Za-z0-9])?$`)
	actorLoginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_\-\[\]]{0,98}[A-Za-z0-9\]])?$`)
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	opaqueIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	returnKeyPattern  = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
)

// AccountIdentity binds a mutable login to GitHub's immutable numeric account
// id. Numeric identity is authoritative; the login prevents confused UI or
// renamed-account behavior.
type AccountIdentity struct {
	ID    int64
	Login string
	Type  string
}

func (a AccountIdentity) validate() error {
	if a.ID <= 0 || !loginPattern.MatchString(a.Login) || (a.Type != "User" && a.Type != "Organization") {
		return errorsIdentity("account")
	}
	return nil
}

func (a AccountIdentity) equal(other AccountIdentity) bool {
	return a.ID == other.ID && strings.EqualFold(a.Login, other.Login) && a.Type == other.Type
}

func (a AccountIdentity) validateActor() error {
	if a.ID <= 0 || !actorLoginPattern.MatchString(a.Login) || (a.Type != "User" && a.Type != "Organization" && a.Type != "Bot") {
		return errorsIdentity("actor")
	}
	return nil
}

// RepositoryIdentity binds repository names to immutable repository and owner
// ids. Every source operation supplies all four values.
type RepositoryIdentity struct {
	ID         int64
	Name       string
	OwnerID    int64
	OwnerLogin string
}

func (r RepositoryIdentity) validate() error {
	if r.ID <= 0 || r.OwnerID <= 0 || !repositoryPattern.MatchString(r.Name) || !loginPattern.MatchString(r.OwnerLogin) ||
		r.Name == "." || r.Name == ".." || strings.HasSuffix(strings.ToLower(r.Name), ".git") {
		return errorsIdentity("repository")
	}
	return nil
}

func (r RepositoryIdentity) fullName() string { return r.OwnerLogin + "/" + r.Name }

func errorsIdentity(kind string) error {
	return fmt.Errorf("%w: %s identity is invalid", ErrInvalidTokenRequest, kind)
}

func validOpaqueID(v string) bool { return opaqueIDPattern.MatchString(v) }
