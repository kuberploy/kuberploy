package githubapp

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	objectIDPattern        = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	builderObjectIDPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type Event interface {
	githubEventName() string
}

// EventInstallationID returns the signed installation id carried by every
// supported event type.
func EventInstallationID(event Event) (int64, bool) {
	switch typed := event.(type) {
	case PushEvent:
		return typed.InstallationID, typed.InstallationID > 0
	case InstallationEvent:
		return typed.InstallationID, typed.InstallationID > 0
	case InstallationRepositoriesEvent:
		return typed.InstallationID, typed.InstallationID > 0
	default:
		return 0, false
	}
}

// PushEvent.After is deliberately named UntrustedAfter: webhook payloads can
// trigger a ref lookup but can never select the source commit. ResolveEventRef
// performs the authoritative GitHub ref lookup.
type PushEvent struct {
	Ref            string
	Created        bool
	Deleted        bool
	Forced         bool
	UntrustedAfter string
	Repository     RepositoryIdentity
	InstallationID int64
	Sender         AccountIdentity
}

func (PushEvent) githubEventName() string { return "push" }

type InstallationEvent struct {
	Action              string
	InstallationID      int64
	Account             AccountIdentity
	RepositorySelection string
	Permissions         Permissions
	SuspendedAt         *time.Time
	Sender              AccountIdentity
}

func (InstallationEvent) githubEventName() string { return "installation" }

type InstallationRepositoriesEvent struct {
	Action              string
	InstallationID      int64
	Account             AccountIdentity
	RepositorySelection string
	Added               []RepositoryIdentity
	Removed             []RepositoryIdentity
	Sender              AccountIdentity
}

func (InstallationRepositoriesEvent) githubEventName() string { return "installation_repositories" }

// ParseEvent parses only supported event families. Unknown event names and
// future unsupported actions are ignored without attempting to decode their
// bodies.
func ParseEvent(envelope WebhookEnvelope) (Event, bool, error) {
	switch envelope.Event {
	case "push":
		event, err := parsePushEvent(envelope.Body)
		return event, err == nil, err
	case "installation":
		event, supported, err := parseInstallationEvent(envelope.Body)
		if !supported {
			return nil, false, err
		}
		return event, supported, err
	case "installation_repositories":
		event, supported, err := parseInstallationRepositoriesEvent(envelope.Body)
		if !supported {
			return nil, false, err
		}
		return event, supported, err
	default:
		return nil, false, nil
	}
}

type webhookUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

type webhookRepository struct {
	ID       int64       `json:"id"`
	Name     string      `json:"name"`
	FullName string      `json:"full_name"`
	Owner    webhookUser `json:"owner"`
}

func (r webhookRepository) identity() (RepositoryIdentity, error) {
	owner, err := r.Owner.accountIdentity()
	if err != nil {
		return RepositoryIdentity{}, err
	}
	identity := RepositoryIdentity{ID: r.ID, Name: r.Name, OwnerID: r.Owner.ID, OwnerLogin: r.Owner.Login}
	if err := identity.validate(); err != nil || owner.ID != identity.OwnerID || !strings.EqualFold(r.FullName, identity.fullName()) {
		return RepositoryIdentity{}, ErrInvalidWebhook
	}
	return identity, nil
}

func (u webhookUser) accountIdentity() (AccountIdentity, error) {
	identity := AccountIdentity{ID: u.ID, Login: u.Login, Type: u.Type}
	if err := identity.validate(); err != nil {
		return AccountIdentity{}, ErrInvalidWebhook
	}
	return identity, nil
}

func (u webhookUser) actorIdentity() (AccountIdentity, error) {
	identity := AccountIdentity{ID: u.ID, Login: u.Login, Type: u.Type}
	if err := identity.validateActor(); err != nil {
		return AccountIdentity{}, ErrInvalidWebhook
	}
	return identity, nil
}

func parsePushEvent(body []byte) (PushEvent, error) {
	var payload struct {
		Ref          string            `json:"ref"`
		After        string            `json:"after"`
		Created      bool              `json:"created"`
		Deleted      bool              `json:"deleted"`
		Forced       bool              `json:"forced"`
		Repository   webhookRepository `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Sender webhookUser `json:"sender"`
	}
	if err := decodeSingleJSON(body, &payload); err != nil {
		return PushEvent{}, fmt.Errorf("%w: push payload is malformed", ErrInvalidWebhook)
	}
	if !validWebhookRef(payload.Ref) || !objectIDPattern.MatchString(payload.After) || payload.Installation.ID <= 0 {
		return PushEvent{}, ErrInvalidWebhook
	}
	allZero := strings.Trim(payload.After, "0") == ""
	if (payload.Deleted && !allZero) || (!payload.Deleted && allZero) || (payload.Created && payload.Deleted) {
		return PushEvent{}, ErrInvalidWebhook
	}
	repository, err := payload.Repository.identity()
	if err != nil {
		return PushEvent{}, err
	}
	sender, err := payload.Sender.actorIdentity()
	if err != nil {
		return PushEvent{}, err
	}
	return PushEvent{
		Ref: payload.Ref, Created: payload.Created, Deleted: payload.Deleted, Forced: payload.Forced,
		UntrustedAfter: payload.After, Repository: repository, InstallationID: payload.Installation.ID, Sender: sender,
	}, nil
}

func validWebhookRef(ref string) bool {
	for _, prefix := range []string{"refs/heads/", "refs/tags/"} {
		if strings.HasPrefix(ref, prefix) {
			return validRefName(strings.TrimPrefix(ref, prefix))
		}
	}
	return false
}

type webhookInstallation struct {
	ID                  int64             `json:"id"`
	Account             webhookUser       `json:"account"`
	RepositorySelection string            `json:"repository_selection"`
	Permissions         map[string]string `json:"permissions"`
	SuspendedAt         *time.Time        `json:"suspended_at"`
}

func parseInstallationEvent(body []byte) (InstallationEvent, bool, error) {
	var payload struct {
		Action       string              `json:"action"`
		Installation webhookInstallation `json:"installation"`
		Sender       webhookUser         `json:"sender"`
	}
	if err := decodeSingleJSON(body, &payload); err != nil {
		return InstallationEvent{}, false, fmt.Errorf("%w: installation payload is malformed", ErrInvalidWebhook)
	}
	switch payload.Action {
	case "created", "deleted", "suspend", "unsuspend", "new_permissions_accepted":
	default:
		return InstallationEvent{}, false, nil
	}
	account, permissions, sender, err := validateWebhookInstallation(payload.Installation, payload.Sender)
	if err != nil {
		return InstallationEvent{}, false, err
	}
	return InstallationEvent{
		Action: payload.Action, InstallationID: payload.Installation.ID, Account: account,
		RepositorySelection: payload.Installation.RepositorySelection, Permissions: permissions,
		SuspendedAt: payload.Installation.SuspendedAt, Sender: sender,
	}, true, nil
}

func parseInstallationRepositoriesEvent(body []byte) (InstallationRepositoriesEvent, bool, error) {
	var payload struct {
		Action              string              `json:"action"`
		Installation        webhookInstallation `json:"installation"`
		RepositorySelection string              `json:"repository_selection"`
		RepositoriesAdded   []webhookRepository `json:"repositories_added"`
		RepositoriesRemoved []webhookRepository `json:"repositories_removed"`
		Sender              webhookUser         `json:"sender"`
	}
	if err := decodeSingleJSON(body, &payload); err != nil {
		return InstallationRepositoriesEvent{}, false, fmt.Errorf("%w: installation repositories payload is malformed", ErrInvalidWebhook)
	}
	if payload.Action != "added" && payload.Action != "removed" {
		return InstallationRepositoriesEvent{}, false, nil
	}
	account, _, sender, err := validateWebhookInstallation(payload.Installation, payload.Sender)
	if err != nil {
		return InstallationRepositoriesEvent{}, false, err
	}
	selection := payload.RepositorySelection
	if selection == "" {
		selection = payload.Installation.RepositorySelection
	}
	if selection != "all" && selection != "selected" {
		return InstallationRepositoriesEvent{}, false, ErrInvalidWebhook
	}
	added, err := webhookRepositoryIdentities(payload.RepositoriesAdded, account)
	if err != nil {
		return InstallationRepositoriesEvent{}, false, err
	}
	removed, err := webhookRepositoryIdentities(payload.RepositoriesRemoved, account)
	if err != nil {
		return InstallationRepositoriesEvent{}, false, err
	}
	if (payload.Action == "added" && (len(added) == 0 || len(removed) != 0)) ||
		(payload.Action == "removed" && (len(removed) == 0 || len(added) != 0)) {
		return InstallationRepositoriesEvent{}, false, ErrInvalidWebhook
	}
	return InstallationRepositoriesEvent{
		Action: payload.Action, InstallationID: payload.Installation.ID, Account: account,
		RepositorySelection: selection, Added: added, Removed: removed, Sender: sender,
	}, true, nil
}

func validateWebhookInstallation(installation webhookInstallation, senderRaw webhookUser) (AccountIdentity, Permissions, AccountIdentity, error) {
	if installation.ID <= 0 || (installation.RepositorySelection != "all" && installation.RepositorySelection != "selected") {
		return AccountIdentity{}, nil, AccountIdentity{}, ErrInvalidWebhook
	}
	account, err := installation.Account.accountIdentity()
	if err != nil {
		return AccountIdentity{}, nil, AccountIdentity{}, err
	}
	sender, err := senderRaw.actorIdentity()
	if err != nil {
		return AccountIdentity{}, nil, AccountIdentity{}, err
	}
	permissions, err := permissionsFromStrings(installation.Permissions, true)
	if err != nil || len(permissions) == 0 {
		return AccountIdentity{}, nil, AccountIdentity{}, ErrInvalidWebhook
	}
	return account, permissions, sender, nil
}

func webhookRepositoryIdentities(raw []webhookRepository, account AccountIdentity) ([]RepositoryIdentity, error) {
	seen := make(map[int64]struct{}, len(raw))
	result := make([]RepositoryIdentity, 0, len(raw))
	for _, repository := range raw {
		identity, err := repository.identity()
		if err != nil || identity.OwnerID != account.ID || !strings.EqualFold(identity.OwnerLogin, account.Login) {
			return nil, ErrInvalidWebhook
		}
		if _, duplicate := seen[identity.ID]; duplicate {
			return nil, ErrInvalidWebhook
		}
		seen[identity.ID] = struct{}{}
		result = append(result, identity)
	}
	return result, nil
}

func permissionsFromStrings(raw map[string]string, allowNone bool) (Permissions, error) {
	result := make(Permissions, len(raw))
	for name, level := range raw {
		result[name] = PermissionLevel(level)
	}
	if err := validatePermissions(result, allowNone); err != nil {
		return nil, err
	}
	return result, nil
}

func validRefName(name string) bool {
	if name == "" || name == "@" || len(name) > 255 || !utf8.ValidString(name) || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") ||
		strings.Contains(name, "//") || strings.Contains(name, "..") || strings.Contains(name, "@{") ||
		strings.HasSuffix(name, ".") || strings.HasSuffix(strings.ToLower(name), ".lock") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(` ~^:?*[\\`, r) {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}
