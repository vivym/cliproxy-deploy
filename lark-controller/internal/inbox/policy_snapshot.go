package inbox

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
)

func (s *Store) SyncPolicySnapshot(ctx context.Context, snapshot policy.Snapshot) error {
	loadedAt := time.Now().UTC()
	policies, bindings, err := validatePolicySnapshot(snapshot, loadedAt)
	if err != nil {
		return err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin policy snapshot transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := loadedAt.Format(time.RFC3339Nano)
	existingPolicies := make(map[string]policy.PolicyState)
	rows, err := tx.QueryContext(ctx, `
SELECT policy_version, catalog_sha256, source_sha256, state, retire_after, catalog_json
FROM policy_versions`)
	if err != nil {
		return fmt.Errorf("read policy snapshots: %w", err)
	}
	for rows.Next() {
		var version string
		var catalogSHA256 string
		var sourceSHA256 string
		var state policy.PolicyState
		var retireAfter string
		var catalogJSON string
		if err := rows.Scan(&version, &catalogSHA256, &sourceSHA256, &state, &retireAfter, &catalogJSON); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan policy snapshot: %w", err)
		}
		incoming, exists := policies[version]
		if !exists {
			_ = rows.Close()
			return fmt.Errorf("policy snapshot removed historical version %q", version)
		}
		if incoming.CatalogSHA256 != catalogSHA256 || incoming.CatalogJSON != catalogJSON {
			_ = rows.Close()
			return fmt.Errorf("policy snapshot mutated immutable version %q", version)
		}
		if !policyStateTransitionAllowed(state, incoming.State) {
			_ = rows.Close()
			return fmt.Errorf("policy %q cannot transition from %q to %q", version, state, incoming.State)
		}
		if state == incoming.State && incoming.SourceSHA256 != sourceSHA256 {
			_ = rows.Close()
			return fmt.Errorf("policy snapshot changed source for unchanged version %q", version)
		}
		if incoming.RetireAfter != retireAfter &&
			!(retireAfter == "" && state == policy.PolicyStateDraining &&
				incoming.State == policy.PolicyStateRetired && incoming.RetireAfter != "") {
			_ = rows.Close()
			return fmt.Errorf("policy %q changed its retirement boundary", version)
		}
		existingPolicies[version] = state
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close policy snapshot rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate policy snapshots: %w", err)
	}
	newPolicyVersions := make(map[string]struct{})
	for version, incoming := range policies {
		storedState, exists := existingPolicies[version]
		if incoming.State == policy.PolicyStateRetired &&
			(!exists || storedState != policy.PolicyStateRetired) {
			if err := ensureNoUnfinishedPolicyApprovals(ctx, tx, version, bindings); err != nil {
				return err
			}
		}
		if exists {
			if _, err := tx.ExecContext(ctx,
				"UPDATE policy_versions SET source_sha256 = ?, state = ?, retire_after = ?, loaded_at = ? WHERE policy_version = ?",
				incoming.SourceSHA256, incoming.State, incoming.RetireAfter, now, version,
			); err != nil {
				return fmt.Errorf("update policy snapshot state: %w", err)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO policy_versions (
    policy_version, catalog_sha256, source_sha256, state, retire_after, catalog_json, loaded_at
) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			version, incoming.CatalogSHA256, incoming.SourceSHA256,
			incoming.State, incoming.RetireAfter, incoming.CatalogJSON, now,
		); err != nil {
			return fmt.Errorf("insert policy snapshot: %w", err)
		}
		newPolicyVersions[version] = struct{}{}
	}

	existingBindings := make(map[string]struct{})
	rows, err = tx.QueryContext(ctx, `
SELECT approval_code, schema_fingerprint, locale, policy_version, approval_kind,
       definition_manifest_sha256, definition_manifest_json,
       accept_instance_started_before
FROM approval_policy_bindings`)
	if err != nil {
		return fmt.Errorf("read approval binding snapshots: %w", err)
	}
	for rows.Next() {
		var stored policy.ApprovalBindingSnapshot
		if err := rows.Scan(
			&stored.ApprovalCode, &stored.SchemaFingerprint, &stored.Locale,
			&stored.PolicyVersion, &stored.ApprovalKind,
			&stored.DefinitionManifestSHA256, &stored.DefinitionManifestJSON,
			&stored.AcceptInstanceStartedBefore,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan approval binding snapshot: %w", err)
		}
		key := approvalBindingSnapshotKey(stored)
		incoming, exists := bindings[key]
		if !exists {
			_ = rows.Close()
			return fmt.Errorf("policy snapshot removed historical approval binding %q", stored.ApprovalCode)
		}
		if incoming.PolicyVersion != stored.PolicyVersion || incoming.ApprovalKind != stored.ApprovalKind ||
			incoming.DefinitionManifestSHA256 != stored.DefinitionManifestSHA256 ||
			incoming.DefinitionManifestJSON != stored.DefinitionManifestJSON {
			_ = rows.Close()
			return fmt.Errorf("policy snapshot mutated approval binding %q", stored.ApprovalCode)
		}
		if stored.AcceptInstanceStartedBefore != incoming.AcceptInstanceStartedBefore &&
			!(stored.AcceptInstanceStartedBefore == "" && incoming.AcceptInstanceStartedBefore != "") {
			_ = rows.Close()
			return fmt.Errorf("policy snapshot reopened or changed approval window %q", stored.ApprovalCode)
		}
		existingBindings[key] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close approval binding rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate approval binding snapshots: %w", err)
	}
	for key, incoming := range bindings {
		if _, exists := existingBindings[key]; exists {
			if _, err := tx.ExecContext(ctx, `
UPDATE approval_policy_bindings
SET accept_instance_started_before = ?, loaded_at = ?
WHERE approval_code = ? AND schema_fingerprint = ? AND locale = ?`,
				incoming.AcceptInstanceStartedBefore, now, incoming.ApprovalCode,
				incoming.SchemaFingerprint, incoming.Locale,
			); err != nil {
				return fmt.Errorf("update approval binding window: %w", err)
			}
			continue
		}
		if _, policyIsNew := newPolicyVersions[incoming.PolicyVersion]; !policyIsNew {
			return fmt.Errorf(
				"cannot attach new approval binding %q to existing policy %q",
				incoming.ApprovalCode,
				incoming.PolicyVersion,
			)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO approval_policy_bindings (
    approval_code, schema_fingerprint, locale, policy_version, approval_kind,
    definition_manifest_sha256, definition_manifest_json,
    accept_instance_started_before, loaded_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			incoming.ApprovalCode, incoming.SchemaFingerprint, incoming.Locale,
			incoming.PolicyVersion, incoming.ApprovalKind,
			incoming.DefinitionManifestSHA256, incoming.DefinitionManifestJSON,
			incoming.AcceptInstanceStartedBefore, now,
		); err != nil {
			return fmt.Errorf("insert approval binding snapshot: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit policy snapshot: %w", err)
	}
	return nil
}

func ensureNoUnfinishedPolicyApprovals(
	ctx context.Context,
	tx *sql.Tx,
	policyVersion string,
	bindings map[string]policy.ApprovalBindingSnapshot,
) error {
	for _, binding := range bindings {
		if binding.PolicyVersion != policyVersion {
			continue
		}
		var unfinished int
		err := tx.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM jobs j
    JOIN lark_event_inbox i ON i.event_key = j.event_key
    WHERE i.approval_code = ? AND j.status IN (?, ?, ?, ?)
)`,
			binding.ApprovalCode,
			jobStatusPending,
			jobStatusProcessing,
			jobStatusRetryWait,
			jobStatusReversalPending,
		).Scan(&unfinished)
		if err != nil {
			return fmt.Errorf("check unfinished approvals for policy %q: %w", policyVersion, err)
		}
		if unfinished != 0 {
			return fmt.Errorf("policy %q has unfinished approval jobs", policyVersion)
		}
	}
	return nil
}

func validatePolicySnapshot(snapshot policy.Snapshot, loadedAt time.Time) (
	map[string]policy.PolicySnapshot,
	map[string]policy.ApprovalBindingSnapshot,
	error,
) {
	if len(snapshot.Policies) == 0 || len(snapshot.Bindings) == 0 {
		return nil, nil, errors.New("policy snapshot requires policies and approval bindings")
	}
	policies := make(map[string]policy.PolicySnapshot, len(snapshot.Policies))
	activePolicies := 0
	for _, item := range snapshot.Policies {
		if item.PolicyVersion == "" || !validSHA256(item.CatalogSHA256) ||
			!validSHA256(item.SourceSHA256) || item.CatalogJSON == "" {
			return nil, nil, errors.New("policy snapshot contains an incomplete policy")
		}
		if item.CatalogSHA256 != sha256String(item.CatalogJSON) {
			return nil, nil, fmt.Errorf("policy %q catalog hash mismatch", item.PolicyVersion)
		}
		if item.State != policy.PolicyStateActive && item.State != policy.PolicyStateDraining &&
			item.State != policy.PolicyStateRetired {
			return nil, nil, fmt.Errorf("policy %q has invalid state %q", item.PolicyVersion, item.State)
		}
		if item.State == policy.PolicyStateRetired {
			retireAfter, err := time.Parse(time.RFC3339, item.RetireAfter)
			if err != nil {
				return nil, nil, fmt.Errorf("retired policy %q requires a valid retire_after", item.PolicyVersion)
			}
			if retireAfter.After(loadedAt) {
				return nil, nil, fmt.Errorf("policy %q cannot retire before %s", item.PolicyVersion, item.RetireAfter)
			}
		} else if item.RetireAfter != "" {
			return nil, nil, fmt.Errorf("non-retired policy %q cannot define retire_after", item.PolicyVersion)
		}
		if item.State == policy.PolicyStateActive {
			activePolicies++
		}
		if _, duplicate := policies[item.PolicyVersion]; duplicate {
			return nil, nil, fmt.Errorf("duplicate policy snapshot %q", item.PolicyVersion)
		}
		policies[item.PolicyVersion] = item
	}
	if activePolicies != 1 {
		return nil, nil, fmt.Errorf("policy snapshot must contain exactly one active version, got %d", activePolicies)
	}
	bindings := make(map[string]policy.ApprovalBindingSnapshot, len(snapshot.Bindings))
	for _, item := range snapshot.Bindings {
		policySnapshot, exists := policies[item.PolicyVersion]
		if item.ApprovalCode == "" || item.Locale == "" || !exists ||
			(item.ApprovalKind != policy.ApprovalKindWalletTopUp &&
				item.ApprovalKind != policy.ApprovalKindSubscriptionLevel) ||
			!validSHA256(item.DefinitionManifestSHA256) ||
			item.DefinitionManifestJSON == "" ||
			item.SchemaFingerprint != "sha256:"+item.DefinitionManifestSHA256 {
			return nil, nil, errors.New("policy snapshot contains an incomplete approval binding")
		}
		if item.DefinitionManifestSHA256 != sha256String(item.DefinitionManifestJSON) {
			return nil, nil, fmt.Errorf("approval %q manifest hash mismatch", item.ApprovalCode)
		}
		if policySnapshot.State == policy.PolicyStateActive && item.AcceptInstanceStartedBefore != "" {
			return nil, nil, fmt.Errorf("active approval %q cannot have a closed acceptance window", item.ApprovalCode)
		}
		if policySnapshot.State != policy.PolicyStateActive && item.AcceptInstanceStartedBefore == "" {
			return nil, nil, fmt.Errorf("historical approval %q requires an acceptance cutoff", item.ApprovalCode)
		}
		if item.AcceptInstanceStartedBefore != "" {
			cutoff, err := time.Parse(time.RFC3339, item.AcceptInstanceStartedBefore)
			if err != nil {
				return nil, nil, fmt.Errorf("approval %q has invalid acceptance cutoff", item.ApprovalCode)
			}
			if policySnapshot.State == policy.PolicyStateRetired {
				retireAfter, _ := time.Parse(time.RFC3339, policySnapshot.RetireAfter)
				if !retireAfter.After(cutoff) {
					return nil, nil, fmt.Errorf(
						"retired policy %q must retire after approval %q cutoff",
						item.PolicyVersion,
						item.ApprovalCode,
					)
				}
			}
		}
		key := approvalBindingSnapshotKey(item)
		if _, duplicate := bindings[key]; duplicate {
			return nil, nil, fmt.Errorf("duplicate approval binding snapshot %q", item.ApprovalCode)
		}
		bindings[key] = item
	}
	return policies, bindings, nil
}

func policyStateTransitionAllowed(from, to policy.PolicyState) bool {
	return from == to ||
		(from == policy.PolicyStateActive && to == policy.PolicyStateDraining) ||
		(from == policy.PolicyStateDraining && to == policy.PolicyStateRetired)
}

func approvalBindingSnapshotKey(binding policy.ApprovalBindingSnapshot) string {
	return binding.ApprovalCode + "\x00" + binding.SchemaFingerprint + "\x00" + binding.Locale
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func sha256String(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:])
}
