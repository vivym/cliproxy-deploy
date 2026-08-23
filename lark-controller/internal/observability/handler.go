package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
)

type SnapshotStore interface {
	OperationalSnapshot(context.Context) (inbox.OperationalSnapshot, error)
}

type Handler struct {
	mode                    string
	store                   SnapshotStore
	readinessMaxReadyJobAge time.Duration
}

func NewHandler(mode string, store SnapshotStore, readinessMaxReadyJobAge time.Duration) (*Handler, error) {
	if mode == "" || store == nil || readinessMaxReadyJobAge <= 0 {
		return nil, errors.New("mode, operational store, and readiness queue age are required")
	}
	return &Handler{
		mode: mode, store: store, readinessMaxReadyJobAge: readinessMaxReadyJobAge,
	}, nil
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("GET /readyz", h.ready)
	mux.HandleFunc("GET /metrics", h.metrics)
}

func (h *Handler) health(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]string{
		"status": "ok",
		"mode":   h.mode,
	})
}

func (h *Handler) ready(response http.ResponseWriter, request *http.Request) {
	snapshot, err := h.store.OperationalSnapshot(request.Context())
	if err != nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready",
			"reason": "store_unavailable",
		})
		return
	}
	if snapshot.OldestReadyJobAge > h.readinessMaxReadyJobAge {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready",
			"reason": "ready_queue_stalled",
		})
		return
	}
	writeJSON(response, http.StatusOK, map[string]string{"status": "ready"})
}

func (h *Handler) metrics(response http.ResponseWriter, request *http.Request) {
	snapshot, err := h.store.OperationalSnapshot(request.Context())
	if err != nil {
		http.Error(response, "metrics unavailable", http.StatusServiceUnavailable)
		return
	}
	response.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var output strings.Builder
	writeCounterMap(&output, "lark_webhook_received_total", "event_type",
		boundedCounts(snapshot.WebhookReceived, allowedEventTypes))
	writeCounterMap(&output, "lark_webhook_duplicate_total", "event_type",
		boundedCounts(snapshot.WebhookDuplicates, allowedEventTypes))
	writeGaugeMap(&output, "lark_controller_inbox_events", "state",
		processingStateCounts(snapshot.InboxStates))
	writeGaugeMap(&output, "lark_controller_jobs", "state",
		boundedCounts(snapshot.JobStates, allowedJobStates))
	writeGaugeMap(&output, "lark_new_api_grant_jobs", "state",
		boundedCounts(snapshot.EntitlementGrantJobStates, allowedEntitlementGrantJobStates))
	writeEntitlementGrantResults(&output, snapshot.EntitlementGrantResults)
	writeCounterMap(&output, "entitlement_grant_retry_total", "reason",
		boundedCounts(snapshot.EntitlementGrantRetries, allowedEntitlementGrantFailureReasons))
	writeCounterMap(&output, "entitlement_dead_letter_total", "reason",
		boundedCounts(snapshot.EntitlementGrantDeadLetters, allowedEntitlementGrantFailureReasons))
	writeGaugeMap(&output, "lark_principal_disable_jobs", "state",
		boundedCounts(snapshot.PrincipalDisableJobStates, allowedPrincipalDisableJobStates))
	writeCounterMap(&output, "principal_disable_total", "result",
		boundedCounts(snapshot.PrincipalDisableResults, allowedPrincipalDisableResults))
	writeCounterMap(&output, "principal_disable_retry_total", "reason",
		boundedCounts(snapshot.PrincipalDisableRetries, allowedPrincipalDisableFailureReasons))
	writeCounterMap(&output, "principal_disable_dead_letter_total", "reason",
		boundedCounts(snapshot.PrincipalDisableDeadLetters, allowedPrincipalDisableFailureReasons))
	writeCounterMap(&output, "employment_reconciliation_total", "result",
		boundedCounts(snapshot.EmploymentReconciliations, allowedEmploymentReconciliationResults))
	writeCounterMap(&output, "lark_controller_processing_recovered_total", "queue",
		boundedCounts(snapshot.ProcessingRecoveries, allowedProcessingRecoveryQueues))
	writeCounterMap(&output, "lark_approval_fetch_total", "result",
		boundedCounts(snapshot.ApprovalFetches, allowedFetchResults))
	writeCounterMap(&output, "lark_new_api_grant_total", "result",
		boundedCounts(snapshot.NewAPIGrants, allowedNewAPIGrantResults))
	writeScalar(&output, "lark_policy_validation_failure_total", "counter",
		float64(snapshot.PolicyValidationFailures))
	writeCounterMap(&output, "lark_controller_dead_letter_total", "reason",
		boundedCounts(snapshot.DeadLetters, allowedDeadLetterReasons))
	writeScalar(&output, "lark_controller_oldest_active_job_age_seconds", "gauge",
		snapshot.OldestActiveJobAge.Seconds())
	writeScalar(&output, "lark_controller_oldest_ready_job_age_seconds", "gauge",
		snapshot.OldestReadyJobAge.Seconds())
	ready := 1.0
	if snapshot.OldestReadyJobAge > h.readinessMaxReadyJobAge {
		ready = 0
	}
	writeScalar(&output, "lark_controller_ready", "gauge", ready)
	_, _ = response.Write([]byte(output.String()))
}

func writeJSON(response http.ResponseWriter, status int, payload map[string]string) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(payload)
}

func writeCounterMap(output *strings.Builder, name, label string, values map[string]int64) {
	writeMap(output, name, "counter", label, values)
}

func writeGaugeMap(output *strings.Builder, name, label string, values map[string]int64) {
	writeMap(output, name, "gauge", label, values)
}

func writeMap(output *strings.Builder, name, metricType, label string, values map[string]int64) {
	fmt.Fprintf(output, "# TYPE %s %s\n", name, metricType)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(output, "%s{%s=\"%s\"} %d\n", name, label, escapeLabel(key), values[key])
	}
}

func writeScalar(output *strings.Builder, name, metricType string, value float64) {
	fmt.Fprintf(output, "# TYPE %s %s\n%s %s\n",
		name, metricType, name, strconv.FormatFloat(value, 'f', -1, 64))
}

func writeEntitlementGrantResults(
	output *strings.Builder,
	values map[inbox.EntitlementGrantResultKey]int64,
) {
	type result struct {
		grantType string
		status    string
		count     int64
	}
	bounded := make(map[inbox.EntitlementGrantResultKey]int64)
	for key, count := range values {
		if _, ok := allowedEntitlementGrantTypes[key.GrantType]; !ok {
			key.GrantType = "other"
		}
		if _, ok := allowedEntitlementGrantStatuses[key.Status]; !ok {
			key.Status = "other"
		}
		bounded[key] += count
	}
	results := make([]result, 0, len(bounded))
	for key, count := range bounded {
		results = append(results, result{grantType: key.GrantType, status: key.Status, count: count})
	}
	sort.Slice(results, func(left, right int) bool {
		if results[left].grantType == results[right].grantType {
			return results[left].status < results[right].status
		}
		return results[left].grantType < results[right].grantType
	})
	output.WriteString("# TYPE entitlement_grant_total counter\n")
	for _, value := range results {
		fmt.Fprintf(
			output,
			"entitlement_grant_total{status=\"%s\",type=\"%s\"} %d\n",
			escapeLabel(value.status),
			escapeLabel(value.grantType),
			value.count,
		)
	}
}

func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return strings.ReplaceAll(value, `"`, `\"`)
}

func processingStateCounts(source map[inbox.ProcessingState]int64) map[string]int64 {
	converted := make(map[string]int64, len(source))
	for state, count := range source {
		converted[string(state)] += count
	}
	return boundedCounts(converted, allowedInboxStates)
}

func boundedCounts(source map[string]int64, allowed map[string]struct{}) map[string]int64 {
	result := make(map[string]int64)
	for key, count := range source {
		if _, ok := allowed[key]; !ok {
			key = "other"
		}
		result[key] += count
	}
	return result
}

func labels(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func entitlementGrantFailureReasonLabels() map[string]struct{} {
	reasons := inbox.EntitlementGrantFailureReasons()
	result := make(map[string]struct{}, len(reasons))
	for _, reason := range reasons {
		result[string(reason)] = struct{}{}
	}
	return result
}

func principalDisableFailureReasonLabels() map[string]struct{} {
	reasons := inbox.PrincipalDisableFailureReasons()
	result := make(map[string]struct{}, len(reasons))
	for _, reason := range reasons {
		result[string(reason)] = struct{}{}
	}
	return result
}

var (
	allowedEventTypes = labels(
		"approval.instance.status_changed_v4",
		"approval.task.status_changed_v4",
		"approval_instance",
		"contact.user.deleted_v3",
	)
	allowedInboxStates                     = labels("pending", "processing", "shadow_recorded", "reversal_pending", "dead_letter", "principal_disabled")
	allowedJobStates                       = labels("pending", "processing", "retry_wait", "succeeded", "reversal_pending", "dead_letter")
	allowedEntitlementGrantJobStates       = labels("held_shadow", "pending", "processing", "retry_wait", "succeeded", "dead_letter")
	allowedEntitlementGrantTypes           = labels("wallet_quota", "subscription_level")
	allowedEntitlementGrantStatuses        = labels("applied", "replayed", "noop", "ignored_stale")
	allowedEntitlementGrantFailureReasons  = entitlementGrantFailureReasonLabels()
	allowedPrincipalDisableJobStates       = labels("held_shadow", "pending", "processing", "retry_wait", "succeeded", "dead_letter")
	allowedPrincipalDisableResults         = labels("applied", "replayed", "noop")
	allowedPrincipalDisableFailureReasons  = principalDisableFailureReasonLabels()
	allowedEmploymentReconciliationResults = labels(
		"success",
		"health_probe_failed",
		"principal_list_failed",
		"employment_check_failed",
		"incomplete_scan",
	)
	allowedProcessingRecoveryQueues = labels(
		inbox.ProcessingRecoveryQueueApproval,
		inbox.ProcessingRecoveryQueueEntitlementGrant,
		inbox.ProcessingRecoveryQueuePrincipalDisable,
	)
	allowedFetchResults       = labels("success", "retryable_error", "terminal_error")
	allowedNewAPIGrantResults = labels(
		"shadow_planned",
		"shadow_replayed",
		"applied",
		"replayed",
		"noop",
		"ignored_stale",
	)
	allowedDeadLetterReasons = labels(
		"dead_letter_unknown_status",
		"dead_letter_unsupported_event_type",
		"dead_letter_policy_validation_failed",
		"rate_limited",
		"server_error",
		"client_error",
		"timeout",
		"transport_error",
		"invalid_response",
		"unclassified_error",
		"invalid_command_plan",
		"external_id_payload_mismatch",
		"retry_exhausted_rate_limited",
		"retry_exhausted_server_error",
		"retry_exhausted_timeout",
		"retry_exhausted_transport_error",
	)
)
