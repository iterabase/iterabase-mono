package identity

import (
	"fmt"
	"sort"
	"strings"
)

// Exactly two customer roles exist (DES-HOR-451-03). `user` is the legacy
// pre-epoch spelling normalized by NormalizeRole.
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
)

// V2 customer API-credential kinds (architecture 7.7). A personal credential is
// issued for and used by one accountable human; an automation credential has a
// required accountable human owner and a distinct, immutable service actor.
const (
	CredentialKindPersonal   = "personal"
	CredentialKindAutomation = "automation"
)

// Credential lifecycle states (architecture 6.5).
const (
	CredentialStatusActive    = "active"
	CredentialStatusRetiring  = "retiring"
	CredentialStatusSuspended = "suspended"
	CredentialStatusExpired   = "expired"
	CredentialStatusRevoked   = "revoked"
)

// V2 authority epochs (architecture 7.11).
const (
	AuthorityEpochLegacy = "legacy"
	AuthorityEpochV2     = "v2"
)

// The complete customer action catalogue (architecture 9.1). There is no
// wildcard, custom string, or implied child action.
const (
	ActionWorkflowsRead       = "workflows.read"
	ActionWorkflowsStart      = "workflows.start"
	ActionWorkRead            = "work.read"
	ActionWorkFeedbackWrite   = "work.feedback.write"
	ActionArtifactsRead       = "artifacts.read"
	ActionArtifactsUpload     = "artifacts.upload"
	ActionArtifactsDelete     = "artifacts.delete"
	ActionInferenceModelsRead = "inference.models.read"
	ActionInferenceChatInvoke = "inference.chat.invoke"
	ActionPeopleRead          = "people.read"
	ActionValueRead           = "value.read"
)

// ActionSpec is one catalogue entry.
type ActionSpec struct {
	Action string
	// Kinds are the credential kinds that may hold the action.
	Kinds []string
	// AdminOnly marks a personal action whose *current* owner must be an Admin.
	// It is never part of the automation catalogue.
	AdminOnly bool
	// Requires lists prerequisite actions that must accompany the action.
	Requires []string
	// RequiresAny lists alternative prerequisites: at least one must accompany
	// the action.
	RequiresAny []string
}

// actionCatalogue is the single in-process authority for the fixed V2 action
// set. The database CHECK constraints in migration 000027 encode the same set
// so neither layer can silently widen the other.
var actionCatalogue = []ActionSpec{
	{Action: ActionWorkflowsRead, Kinds: []string{CredentialKindPersonal, CredentialKindAutomation}},
	{Action: ActionWorkflowsStart, Kinds: []string{CredentialKindPersonal, CredentialKindAutomation}},
	{Action: ActionWorkRead, Kinds: []string{CredentialKindPersonal, CredentialKindAutomation}},
	{Action: ActionWorkFeedbackWrite, Kinds: []string{CredentialKindPersonal}},
	{Action: ActionArtifactsRead, Kinds: []string{CredentialKindPersonal, CredentialKindAutomation}},
	{Action: ActionArtifactsUpload, Kinds: []string{CredentialKindPersonal, CredentialKindAutomation}},
	{
		Action:    ActionArtifactsDelete,
		Kinds:     []string{CredentialKindPersonal},
		AdminOnly: true,
		Requires:  []string{ActionArtifactsRead},
	},
	{Action: ActionInferenceModelsRead, Kinds: []string{CredentialKindPersonal, CredentialKindAutomation}},
	{Action: ActionInferenceChatInvoke, Kinds: []string{CredentialKindPersonal, CredentialKindAutomation}},
	{Action: ActionPeopleRead, Kinds: []string{CredentialKindPersonal}, AdminOnly: true},
	{
		Action:      ActionValueRead,
		Kinds:       []string{CredentialKindPersonal},
		AdminOnly:   true,
		RequiresAny: []string{ActionWorkRead, ActionWorkflowsRead},
	},
}

// ActionFor returns the catalogue entry for an action.
func ActionFor(action string) (ActionSpec, bool) {
	for _, spec := range actionCatalogue {
		if spec.Action == action {
			return spec, true
		}
	}
	return ActionSpec{}, false
}

// KnownAction reports whether the action is in the fixed V2 catalogue.
func KnownAction(action string) bool {
	_, ok := ActionFor(action)
	return ok
}

// ActionAdminOnly reports whether the action is gated on a current Admin.
func ActionAdminOnly(action string) bool {
	spec, ok := ActionFor(action)
	return ok && spec.AdminOnly
}

// CatalogueActions returns the actions a credential kind may hold, sorted.
func CatalogueActions(kind string) []string {
	var out []string
	for _, spec := range actionCatalogue {
		for _, candidate := range spec.Kinds {
			if candidate == kind {
				out = append(out, spec.Action)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// subsetOfActions reports whether every action is a member of allowed.
func subsetOfActions(actions, allowed []string) bool {
	permitted := make(map[string]struct{}, len(allowed))
	for _, action := range allowed {
		permitted[action] = struct{}{}
	}
	for _, action := range actions {
		if _, ok := permitted[action]; !ok {
			return false
		}
	}
	return true
}

// ValidateActions enforces the fixed catalogue, the credential-kind boundary,
// the Admin-only boundary, and prerequisite membership. ownerRole is the
// *current* role of the accountable owner; Admin-only actions require it to be
// `admin` at creation time and are re-checked on every request.
//
// Membership is validated in one pass and prerequisites in a second pass, so a
// set is accepted or rejected on its contents alone and never on slice order.
// ActionsSatisfied (the request-path matcher) is order-independent for the same
// reason; the two layers must never disagree about an identical set.
//
//nolint:gocyclo // One auditable gate: membership, kind boundary, Admin gating, then prerequisites.
func ValidateActions(kind string, actions []string, ownerRole string) error {
	if kind != CredentialKindPersonal && kind != CredentialKindAutomation {
		return fmt.Errorf("identity: invalid credential kind %q", kind)
	}
	if len(actions) == 0 {
		return fmt.Errorf("identity: at least one action is required")
	}

	present := make(map[string]struct{}, len(actions))
	for _, action := range actions {
		if action == "" || strings.TrimSpace(action) != action {
			return fmt.Errorf("identity: invalid action %q", action)
		}
		if _, dup := present[action]; dup {
			return fmt.Errorf("identity: duplicate action %q", action)
		}
		present[action] = struct{}{}

		spec, ok := ActionFor(action)
		if !ok {
			return fmt.Errorf("identity: unknown action %q", action)
		}
		allowed := false
		for _, candidate := range spec.Kinds {
			if candidate == kind {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("identity: action %q is not available to %s credentials", action, kind)
		}
		if spec.AdminOnly && ownerRole != RoleAdmin {
			return fmt.Errorf("identity: action %q requires a current Admin owner", action)
		}
	}

	for _, action := range actions {
		spec, _ := ActionFor(action)
		for _, required := range spec.Requires {
			if _, ok := present[required]; !ok {
				return fmt.Errorf("identity: action %q requires %q", action, required)
			}
		}
		if len(spec.RequiresAny) > 0 {
			satisfied := false
			for _, alternative := range spec.RequiresAny {
				if _, ok := present[alternative]; ok {
					satisfied = true
					break
				}
			}
			if !satisfied {
				return fmt.Errorf("identity: action %q requires one of %s", action, strings.Join(spec.RequiresAny, ", "))
			}
		}
	}
	return nil
}

// ActionsSatisfied reports whether the granted set authorizes the required
// action, re-applying the prerequisite contract for sets validated at creation.
func ActionsSatisfied(granted []string, required string) bool {
	spec, ok := ActionFor(required)
	if !ok {
		return false
	}
	have := make(map[string]struct{}, len(granted))
	for _, action := range granted {
		have[action] = struct{}{}
	}
	if _, ok := have[required]; !ok {
		return false
	}
	for _, prerequisite := range spec.Requires {
		if _, ok := have[prerequisite]; !ok {
			return false
		}
	}
	if len(spec.RequiresAny) > 0 {
		satisfied := false
		for _, alternative := range spec.RequiresAny {
			if _, ok := have[alternative]; ok {
				satisfied = true
				break
			}
		}
		if !satisfied {
			return false
		}
	}
	return true
}
