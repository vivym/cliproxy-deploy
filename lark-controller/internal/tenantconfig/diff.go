package tenantconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

type ObservedState struct {
	LocalArtifacts map[string]string `json:"local_artifacts,omitempty"`
	NewAPI         *ObservedNewAPI   `json:"new_api,omitempty"`
	Lark           *ObservedLark     `json:"lark,omitempty"`
}

type ObservedNewAPI struct {
	PolicyCatalogs      map[string]string        `json:"policy_catalogs"`
	PolicyStates        map[string]string        `json:"policy_states"`
	ActivePolicyVersion string                   `json:"active_policy_version,omitempty"`
	OAuthProvider       *OAuthProviderProjection `json:"oauth_provider,omitempty"`
	OAuthPreflight      *ObservedOAuthPreflight  `json:"oauth_preflight"`
}

type ObservedOAuthPreflight struct {
	ChangeRequired bool   `json:"change_required"`
	CurrentDigest  string `json:"current_digest,omitempty"`
	DesiredDigest  string `json:"desired_digest"`
}

type OAuthProviderMutation struct {
	Projection            OAuthProviderProjection `json:"projection"`
	ExpectedCurrentDigest string                  `json:"expected_current_digest,omitempty"`
	DesiredDigest         string                  `json:"desired_digest"`
}

type ObservedLark struct {
	AppID                 string            `json:"app_id"`
	TenantKey             string            `json:"tenant_key"`
	ApprovalFingerprints  map[string]string `json:"approval_fingerprints"`
	RedirectURLs          map[string]bool   `json:"redirect_urls"`
	ConsoleEvents         map[string]bool   `json:"console_events"`
	ApprovalSubscriptions map[string]bool   `json:"approval_subscriptions"`
}

type ChangePlan struct {
	FormatVersion   int            `json:"format_version"`
	CompiledDigest  string         `json:"compiled_digest"`
	ObservedTargets []ChangeTarget `json:"observed_targets"`
	Digest          string         `json:"digest"`
	Changes         []Change       `json:"changes"`
	Blockers        []Blocker      `json:"blockers"`
}

type ChangeSequence int
type ChangeTarget string
type ChangeAction string

const (
	SequenceLarkSubscription ChangeSequence = 10
	SequenceNewAPIStage      ChangeSequence = 20
	SequenceLocalWrite       ChangeSequence = 30
	SequenceNewAPIActivation ChangeSequence = 40

	TargetLark   ChangeTarget = "lark"
	TargetNewAPI ChangeTarget = "new-api"
	TargetLocal  ChangeTarget = "local"

	ActionSubscribeApproval ChangeAction = "subscribe-approval"
	ActionPublishPolicy     ChangeAction = "publish"
	ActionUpsertDisabled    ChangeAction = "upsert-disabled"
	ActionWriteArtifact     ChangeAction = "write"
	ActionActivatePolicy    ChangeAction = "activate"
)

type Change struct {
	ID            string         `json:"id"`
	Sequence      ChangeSequence `json:"sequence"`
	Target        ChangeTarget   `json:"target"`
	Action        ChangeAction   `json:"action"`
	Resource      string         `json:"resource"`
	BeforeDigest  string         `json:"before_digest,omitempty"`
	DesiredDigest string         `json:"desired_digest"`
	PayloadSHA256 string         `json:"payload_sha256"`
	Payload       []byte         `json:"payload"`
}

func (change Change) ValidOperation() bool {
	switch {
	case change.Sequence == SequenceLarkSubscription &&
		change.Target == TargetLark && change.Action == ActionSubscribeApproval:
		return true
	case change.Sequence == SequenceNewAPIStage && change.Target == TargetNewAPI &&
		(change.Action == ActionPublishPolicy || change.Action == ActionUpsertDisabled):
		return true
	case change.Sequence == SequenceLocalWrite &&
		change.Target == TargetLocal && change.Action == ActionWriteArtifact:
		return true
	case change.Sequence == SequenceNewAPIActivation &&
		change.Target == TargetNewAPI && change.Action == ActionActivatePolicy:
		return true
	default:
		return false
	}
}

type Blocker struct {
	Code     string `json:"code"`
	Resource string `json:"resource"`
	Message  string `json:"message"`
}

func Diff(compiled CompiledBundle, observed ObservedState) (ChangePlan, error) {
	if compiled.Digest == "" || len(compiled.Artifacts) == 0 {
		return ChangePlan{}, errors.New("compiled bundle is required")
	}
	plan := ChangePlan{
		FormatVersion: supportedFormatVersion, CompiledDigest: compiled.Digest,
		ObservedTargets: []ChangeTarget{TargetLocal},
	}

	for _, artifact := range compiled.Artifacts {
		before := observed.LocalArtifacts[artifact.Path]
		if before == artifact.SHA256 {
			continue
		}
		plan.Changes = append(plan.Changes, Change{
			ID: "local:write:" + artifact.Path, Sequence: SequenceLocalWrite,
			Target: TargetLocal, Action: ActionWriteArtifact,
			Resource: artifact.Path, BeforeDigest: before, DesiredDigest: artifact.SHA256,
			PayloadSHA256: artifact.SHA256, Payload: append([]byte(nil), artifact.Contents...),
		})
	}

	if observed.NewAPI != nil {
		plan.ObservedTargets = append(plan.ObservedTargets, TargetNewAPI)
		if err := diffNewAPI(compiled, observed.NewAPI, &plan); err != nil {
			return ChangePlan{}, err
		}
	} else {
		plan.Blockers = append(plan.Blockers, Blocker{
			Code: "remote_observation_missing", Resource: string(TargetNewAPI),
			Message: "New API must be observed through the isolated configuration endpoint",
		})
	}
	if observed.Lark != nil {
		plan.ObservedTargets = append(plan.ObservedTargets, TargetLark)
		if err := diffLark(compiled, observed.Lark, &plan); err != nil {
			return ChangePlan{}, err
		}
	} else {
		plan.Blockers = append(plan.Blockers, Blocker{
			Code: "remote_observation_missing", Resource: string(TargetLark),
			Message: "Lark must be observed through reviewed attestation and live approval queries",
		})
	}

	sort.Slice(plan.Changes, func(left, right int) bool {
		if plan.Changes[left].Sequence != plan.Changes[right].Sequence {
			return plan.Changes[left].Sequence < plan.Changes[right].Sequence
		}
		return plan.Changes[left].ID < plan.Changes[right].ID
	})
	sort.Slice(plan.Blockers, func(left, right int) bool {
		if plan.Blockers[left].Code != plan.Blockers[right].Code {
			return plan.Blockers[left].Code < plan.Blockers[right].Code
		}
		return plan.Blockers[left].Resource < plan.Blockers[right].Resource
	})
	digest, err := planDigest(plan)
	if err != nil {
		return ChangePlan{}, err
	}
	plan.Digest = digest
	return plan, nil
}

func diffNewAPI(compiled CompiledBundle, observed *ObservedNewAPI, plan *ChangePlan) error {
	publicationArtifact, err := compiled.Artifact("new-api/policy-publication.json")
	if err != nil {
		return err
	}
	var publication policyPublication
	if err := json.Unmarshal(publicationArtifact.Contents, &publication); err != nil {
		return fmt.Errorf("decode compiled New API publication: %w", err)
	}
	observedHash := observed.PolicyCatalogs[publication.PolicyVersion]
	observedState := observed.PolicyStates[publication.PolicyVersion]
	activationAllowed := true
	switch {
	case observedHash == "":
		plan.Changes = append(plan.Changes, Change{
			ID: "new-api:publish:" + publication.PolicyVersion, Sequence: SequenceNewAPIStage,
			Target: TargetNewAPI, Action: ActionPublishPolicy,
			Resource: publication.PolicyVersion, DesiredDigest: publication.CatalogHash,
			PayloadSHA256: publicationArtifact.SHA256, Payload: append([]byte(nil), publicationArtifact.Contents...),
		})
	case observedHash != publication.CatalogHash:
		plan.Blockers = append(plan.Blockers, Blocker{
			Code: "new_api_policy_immutable_drift", Resource: publication.PolicyVersion,
			Message: "New API already contains this policy version with a different catalog hash",
		})
		activationAllowed = false
	case observed.ActivePolicyVersion == publication.PolicyVersion:
		if observedState != "active" {
			return errors.New("New API active policy has an inconsistent lifecycle state")
		}
	case observedState != "staged":
		plan.Blockers = append(plan.Blockers, Blocker{
			Code: "new_api_policy_not_staged", Resource: publication.PolicyVersion,
			Message: "New API policy exists but is not staged and cannot be activated",
		})
		activationAllowed = false
	}

	activationArtifact, err := compiled.Artifact("new-api/policy-activation.json")
	if err != nil {
		return err
	}
	var activation policyActivation
	if err := json.Unmarshal(activationArtifact.Contents, &activation); err != nil {
		return fmt.Errorf("decode compiled New API activation: %w", err)
	}
	if activationAllowed && observed.ActivePolicyVersion != activation.PolicyVersion {
		if observed.ActivePolicyVersion != activation.ExpectedActivePolicyVersion {
			plan.Blockers = append(plan.Blockers, Blocker{
				Code: "new_api_active_policy_drift", Resource: activation.PolicyVersion,
				Message: "observed active policy does not match the activation expectation",
			})
		} else {
			plan.Changes = append(plan.Changes, Change{
				ID: "new-api:activate:" + activation.PolicyVersion, Sequence: SequenceNewAPIActivation,
				Target: TargetNewAPI, Action: ActionActivatePolicy, Resource: activation.PolicyVersion,
				BeforeDigest: observed.ActivePolicyVersion, DesiredDigest: publication.CatalogHash,
				PayloadSHA256: activationArtifact.SHA256, Payload: append([]byte(nil), activationArtifact.Contents...),
			})
		}
	}

	oauthArtifact, err := compiled.Artifact("new-api/oauth-provider.json")
	if err != nil {
		return err
	}
	if observed.OAuthPreflight == nil {
		plan.Blockers = append(plan.Blockers, Blocker{
			Code: "new_api_oauth_preflight_missing", Resource: "lark",
			Message: "New API must preflight the managed OAuth provider with the mounted bridge secret",
		})
		return nil
	}
	oauthPreflight := observed.OAuthPreflight
	if !validHexDigest(oauthPreflight.DesiredDigest) ||
		(oauthPreflight.CurrentDigest != "" && !validHexDigest(oauthPreflight.CurrentDigest)) ||
		oauthPreflight.ChangeRequired == (oauthPreflight.CurrentDigest == oauthPreflight.DesiredDigest) {
		return errors.New("New API OAuth provider preflight contains inconsistent digests")
	}
	if oauthPreflight.ChangeRequired {
		var projection OAuthProviderProjection
		if err := json.Unmarshal(oauthArtifact.Contents, &projection); err != nil {
			return fmt.Errorf("decode compiled New API OAuth provider: %w", err)
		}
		payload, err := json.Marshal(OAuthProviderMutation{
			Projection: projection, ExpectedCurrentDigest: oauthPreflight.CurrentDigest,
			DesiredDigest: oauthPreflight.DesiredDigest,
		})
		if err != nil {
			return fmt.Errorf("encode New API OAuth provider mutation: %w", err)
		}
		plan.Changes = append(plan.Changes, Change{
			ID: "new-api:upsert-disabled:lark", Sequence: SequenceNewAPIStage,
			Target: TargetNewAPI, Action: ActionUpsertDisabled,
			Resource: "lark", BeforeDigest: oauthPreflight.CurrentDigest,
			DesiredDigest: oauthPreflight.DesiredDigest,
			PayloadSHA256: sha256Hex(payload), Payload: payload,
		})
	}
	return nil
}

func diffLark(compiled CompiledBundle, observed *ObservedLark, plan *ChangePlan) error {
	preflightArtifact, err := compiled.Artifact("lark/tenant-preflight.json")
	if err != nil {
		return err
	}
	var desired LarkTenantPreflight
	if err := json.Unmarshal(preflightArtifact.Contents, &desired); err != nil {
		return fmt.Errorf("decode compiled Lark preflight: %w", err)
	}
	if observed.AppID != desired.AppID || observed.TenantKey != desired.TenantKey {
		plan.Blockers = append(plan.Blockers, Blocker{
			Code: "lark_tenant_identity_mismatch", Resource: "tenant",
			Message: "observed Lark application or tenant identity does not match the environment binding",
		})
	}
	for _, redirectURL := range missingSetMembers(desired.RedirectURLs, observed.RedirectURLs) {
		plan.Blockers = append(plan.Blockers, Blocker{
			Code: "lark_redirect_url_not_attested", Resource: redirectURL,
			Message: "the Lark OAuth redirect URL must be explicitly reviewed and attested",
		})
	}
	for _, redirectURL := range unexpectedSetMembers(desired.RedirectURLs, observed.RedirectURLs) {
		plan.Blockers = append(plan.Blockers, Blocker{
			Code: "lark_redirect_url_unexpected", Resource: redirectURL,
			Message: "the Lark console attestation contains an unexpected OAuth redirect URL",
		})
	}
	for _, definition := range desired.ApprovalDefinitions {
		fingerprint, exists := observed.ApprovalFingerprints[definition.ApprovalCode]
		switch {
		case !exists:
			plan.Blockers = append(plan.Blockers, Blocker{
				Code: "lark_approval_definition_missing", Resource: definition.ApprovalCode,
				Message: "the bound Lark approval definition must be created and reviewed before apply",
			})
		case fingerprint != definition.SchemaFingerprint:
			plan.Blockers = append(plan.Blockers, Blocker{
				Code: "lark_approval_definition_drift", Resource: definition.ApprovalCode,
				Message: "the Lark approval definition does not match the compiled manifest fingerprint",
			})
		}
	}
	for _, event := range missingSetMembers(desired.ConsoleEvents, observed.ConsoleEvents) {
		plan.Blockers = append(plan.Blockers, Blocker{
			Code: "lark_console_event_not_attested", Resource: event,
			Message: "the Lark developer console event must be explicitly reviewed and attested",
		})
	}
	for _, event := range unexpectedSetMembers(desired.ConsoleEvents, observed.ConsoleEvents) {
		plan.Blockers = append(plan.Blockers, Blocker{
			Code: "lark_console_event_unexpected", Resource: event,
			Message: "the Lark console attestation contains an unexpected event subscription",
		})
	}
	desiredSubscriptions := make([]string, 0, len(desired.ApprovalSubscriptions))
	for _, subscription := range desired.ApprovalSubscriptions {
		desiredSubscriptions = append(desiredSubscriptions, subscription.ApprovalCode)
	}
	for _, approvalCode := range unexpectedSetMembers(desiredSubscriptions, observed.ApprovalSubscriptions) {
		plan.Blockers = append(plan.Blockers, Blocker{
			Code: "lark_approval_subscription_unexpected", Resource: approvalCode,
			Message: "the Lark console attestation contains an unexpected approval subscription",
		})
	}
	for _, subscription := range desired.ApprovalSubscriptions {
		if observed.ApprovalSubscriptions[subscription.ApprovalCode] {
			continue
		}
		payload, err := json.Marshal(LarkApprovalSubscriptionMutation{
			ApprovalCode: subscription.ApprovalCode,
			AppID:        desired.AppID,
			TenantKey:    desired.TenantKey,
		})
		if err != nil {
			return fmt.Errorf("encode Lark approval subscription: %w", err)
		}
		plan.Changes = append(plan.Changes, Change{
			ID: "lark:subscribe-approval:" + subscription.ApprovalCode, Sequence: SequenceLarkSubscription,
			Target: TargetLark, Action: ActionSubscribeApproval, Resource: subscription.ApprovalCode,
			DesiredDigest: sha256Hex(payload), PayloadSHA256: sha256Hex(payload), Payload: payload,
		})
	}
	return nil
}

func missingSetMembers(desired []string, observed map[string]bool) []string {
	missing := make([]string, 0)
	for _, value := range desired {
		if !observed[value] {
			missing = append(missing, value)
		}
	}
	sort.Strings(missing)
	return missing
}

func unexpectedSetMembers(desired []string, observed map[string]bool) []string {
	desiredSet := make(map[string]struct{}, len(desired))
	for _, value := range desired {
		desiredSet[value] = struct{}{}
	}
	unexpected := make([]string, 0)
	for value, present := range observed {
		if _, allowed := desiredSet[value]; present && !allowed {
			unexpected = append(unexpected, value)
		}
	}
	sort.Strings(unexpected)
	return unexpected
}

func planDigest(plan ChangePlan) (string, error) {
	authority := struct {
		FormatVersion   int            `json:"format_version"`
		CompiledDigest  string         `json:"compiled_digest"`
		ObservedTargets []ChangeTarget `json:"observed_targets"`
		Changes         []Change       `json:"changes"`
		Blockers        []Blocker      `json:"blockers"`
	}{
		FormatVersion: plan.FormatVersion, CompiledDigest: plan.CompiledDigest,
		ObservedTargets: plan.ObservedTargets, Changes: plan.Changes, Blockers: plan.Blockers,
	}
	contents, err := json.Marshal(authority)
	if err != nil {
		return "", fmt.Errorf("encode change plan digest authority: %w", err)
	}
	return "sha256:" + sha256Hex(contents), nil
}
