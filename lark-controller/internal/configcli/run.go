package configcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/strictjson"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/tenantconfig"
)

func Run(ctx context.Context, arguments []string, output, errorOutput io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("a command is required: init, check, plan, or apply")
	}
	switch arguments[0] {
	case "init":
		return runInit(arguments[1:], output, errorOutput)
	case "check":
		return runCheck(arguments[1:], output, errorOutput)
	case "plan":
		return runPlan(arguments[1:], output, errorOutput)
	case "apply":
		return runApply(ctx, arguments[1:], output, errorOutput)
	case "help", "--help", "-h":
		_, _ = fmt.Fprintln(output, "usage: lark-config <init|check|plan|apply> [options]")
		return nil
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

type compileOptions struct {
	sourcePath  string
	bindingPath string
}

func addCompileFlags(flags *flag.FlagSet, options *compileOptions) {
	flags.StringVar(&options.sourcePath, "source", "lark-runtime/config/policy.json", "policy source JSON file")
	flags.StringVar(&options.bindingPath, "binding", "lark-runtime/config/production.binding.json", "environment binding JSON file")
}

func runCheck(arguments []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("lark-config check", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	var options compileOptions
	addCompileFlags(flags, &options)
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("check does not accept positional arguments")
	}
	compiled, err := loadAndCompile(options)
	if err != nil {
		return err
	}
	return writeJSON(output, struct {
		Status         string `json:"status"`
		CompiledDigest string `json:"compiled_digest"`
		ArtifactCount  int    `json:"artifact_count"`
	}{Status: "valid", CompiledDigest: compiled.Digest, ArtifactCount: len(compiled.Artifacts)})
}

func runPlan(arguments []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("lark-config plan", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	var options compileOptions
	addCompileFlags(flags, &options)
	var outputRoot string
	var planPath string
	var remote string
	var newAPIBaseURL string
	var newAPIConfigSecretFile string
	var larkCLIExecutable string
	var larkConsoleAttestation string
	flags.StringVar(&outputRoot, "output-root", "lark-runtime", "runtime artifact root")
	flags.StringVar(&planPath, "plan", "lark-runtime/ops/lark-config-plan.json", "change plan output file")
	flags.StringVar(&remote, "remote", "lark,new-api", "required remote observations: lark,new-api")
	flags.StringVar(&newAPIBaseURL, "new-api-base-url", "", "New API isolated configuration origin")
	flags.StringVar(&newAPIConfigSecretFile, "new-api-config-secret-file", "", "New API configuration credential file")
	flags.StringVar(&larkCLIExecutable, "lark-cli", "lark-cli", "lark-cli executable")
	flags.StringVar(&larkConsoleAttestation, "lark-console-attestation", "", "reviewed Lark console event attestation JSON")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("plan does not accept positional arguments")
	}
	remoteTargets, err := parseRemoteTargets(remote)
	if err != nil {
		return err
	}
	compiled, err := loadAndCompile(options)
	if err != nil {
		return err
	}
	observed, err := observeLocal(outputRoot, compiled)
	if err != nil {
		return err
	}
	if remoteTargets["new-api"] {
		client, err := newNewAPIConfigurationClient(newAPIBaseURL, newAPIConfigSecretFile)
		if err != nil {
			return err
		}
		observed.NewAPI, err = client.observe(context.Background(), compiled)
		if err != nil {
			return err
		}
	}
	if remoteTargets["lark"] {
		client, err := newLarkCLIClient(larkCLIExecutable, larkConsoleAttestation, execLarkCommandRunner{})
		if err != nil {
			return err
		}
		observed.Lark, err = client.observe(context.Background(), compiled)
		if err != nil {
			return err
		}
	}
	plan, err := tenantconfig.Diff(compiled, observed)
	if err != nil {
		return err
	}
	contents, err := marshalDocument(plan)
	if err != nil {
		return fmt.Errorf("encode change plan: %w", err)
	}
	if err := atomicWriteWithin(outputRoot, planPath, contents, 0o600); err != nil {
		return fmt.Errorf("write change plan: %w", err)
	}
	return writeJSON(output, struct {
		Status       string `json:"status"`
		PlanDigest   string `json:"plan_digest"`
		ChangeCount  int    `json:"change_count"`
		BlockerCount int    `json:"blocker_count"`
	}{Status: "planned", PlanDigest: plan.Digest, ChangeCount: len(plan.Changes), BlockerCount: len(plan.Blockers)})
}

func runApply(ctx context.Context, arguments []string, output, errorOutput io.Writer) (returnErr error) {
	flags := flag.NewFlagSet("lark-config apply", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	var planPath string
	var outputRoot string
	var receiptPath string
	var expectedDigest string
	var changeTicket string
	var newAPIBaseURL string
	var newAPIConfigSecretFile string
	var larkCLIExecutable string
	flags.StringVar(&planPath, "plan", "", "reviewed change plan JSON file")
	flags.StringVar(&outputRoot, "output-root", "lark-runtime", "runtime artifact root")
	flags.StringVar(&receiptPath, "receipt", "", "apply receipt output file")
	flags.StringVar(&expectedDigest, "expected-digest", "", "reviewed plan digest")
	flags.StringVar(&changeTicket, "change-ticket", "", "operator change ticket")
	flags.StringVar(&newAPIBaseURL, "new-api-base-url", "", "New API isolated configuration origin")
	flags.StringVar(&newAPIConfigSecretFile, "new-api-config-secret-file", "", "New API configuration credential file")
	flags.StringVar(&larkCLIExecutable, "lark-cli", "lark-cli", "lark-cli executable")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("apply does not accept positional arguments")
	}
	if planPath == "" || expectedDigest == "" || changeTicket == "" {
		return errors.New("apply requires --plan, --expected-digest, and --change-ticket")
	}
	if receiptPath == "" {
		receiptPath = planPath + ".receipt.json"
	}
	managedPlanPath, err := managedPath(outputRoot, planPath)
	if err != nil {
		return fmt.Errorf("validate change plan path: %w", err)
	}
	if err := ensureSafeParent(outputRoot, filepath.Dir(managedPlanPath)); err != nil {
		return fmt.Errorf("validate change plan parent: %w", err)
	}
	contents, exists, err := readRegularFile(managedPlanPath)
	if err != nil || !exists {
		return fmt.Errorf("read change plan: path must be a readable regular non-symlink file")
	}
	managedReceiptPath, err := preflightManagedOutputPath(outputRoot, receiptPath, 0o600)
	if err != nil {
		return fmt.Errorf("validate apply receipt path: %w", err)
	}
	if sameManagedFile(managedPlanPath, managedReceiptPath) {
		return errors.New("apply receipt path must be different from the reviewed plan")
	}
	var plan tenantconfig.ChangePlan
	if err := strictjson.Decode(contents, &plan); err != nil {
		return fmt.Errorf("decode change plan: %w", err)
	}
	boundary, err := acquireConfigurationMaintenanceBoundary(outputRoot)
	if err != nil {
		return err
	}
	defer func() {
		if err := boundary.Release(); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("release configuration maintenance boundary: %w", err)
		}
	}()
	executor := &targetExecutor{
		local:       &localExecutor{root: outputRoot},
		maintenance: boundary,
	}
	if planHasTarget(plan, "new-api") {
		executor.newAPI, err = newNewAPIConfigurationClient(newAPIBaseURL, newAPIConfigSecretFile)
		if err != nil {
			return err
		}
	}
	if planHasTarget(plan, "lark") {
		executor.lark, err = newLarkCLIClient(larkCLIExecutable, "", execLarkCommandRunner{})
		if err != nil {
			return err
		}
	}
	receipt, applyErr := tenantconfig.Apply(ctx, plan, expectedDigest, tenantconfig.ApplyOptions{
		ChangeTicket: changeTicket,
		Executor:     executor,
	})
	if receipt.Digest != "" {
		if err := boundary.Verify(); err != nil {
			return err
		}
		receiptContents, err := marshalDocument(receipt)
		if err != nil {
			return fmt.Errorf("encode apply receipt: %w", err)
		}
		if err := atomicWrite(managedReceiptPath, receiptContents, 0o600); err != nil {
			return fmt.Errorf("write apply receipt: %w", err)
		}
	}
	if applyErr != nil {
		return applyErr
	}
	return writeJSON(output, struct {
		Status        tenantconfig.ApplyStatus `json:"status"`
		ReceiptDigest string                   `json:"receipt_digest"`
	}{Status: receipt.Status, ReceiptDigest: receipt.Digest})
}

type targetExecutor struct {
	local       tenantconfig.Executor
	newAPI      tenantconfig.Executor
	lark        tenantconfig.Executor
	maintenance *configurationMaintenanceBoundary
}

func (executor *targetExecutor) Execute(ctx context.Context, change tenantconfig.Change) (tenantconfig.ExecutionResult, error) {
	if executor.maintenance == nil {
		return tenantconfig.ExecutionResult{}, errors.New("configuration maintenance boundary is required")
	}
	if err := executor.maintenance.Verify(); err != nil {
		return tenantconfig.ExecutionResult{}, err
	}
	switch change.Target {
	case tenantconfig.TargetLocal:
		return executor.local.Execute(ctx, change)
	case tenantconfig.TargetNewAPI:
		if executor.newAPI == nil {
			return tenantconfig.ExecutionResult{}, errors.New("New API executor is not configured")
		}
		return executor.newAPI.Execute(ctx, change)
	case tenantconfig.TargetLark:
		if executor.lark == nil {
			return tenantconfig.ExecutionResult{}, errors.New("Lark executor is not configured")
		}
		return executor.lark.Execute(ctx, change)
	default:
		return tenantconfig.ExecutionResult{}, errors.New("remote operation requires an explicit remote executor")
	}
}

func planHasTarget(plan tenantconfig.ChangePlan, target tenantconfig.ChangeTarget) bool {
	for _, change := range plan.Changes {
		if change.Target == target {
			return true
		}
	}
	return false
}

func parseRemoteTargets(raw string) (map[string]bool, error) {
	if raw == "none" {
		return map[string]bool{}, nil
	}
	targets := make(map[string]bool)
	for _, target := range strings.Split(raw, ",") {
		if target != "lark" && target != "new-api" || targets[target] {
			return nil, errors.New("remote must be none, lark, new-api, or lark,new-api")
		}
		targets[target] = true
	}
	return targets, nil
}

func readSecretToken(path string, minimum, maximum int) (string, error) {
	if path == "" {
		return "", errors.New("secret file path is required")
	}
	contents, exists, err := readRegularFile(path)
	if err != nil || !exists {
		return "", errors.New("secret file must be a readable regular non-symlink file")
	}
	if bytes.HasSuffix(contents, []byte{'\r', '\n'}) {
		contents = bytes.TrimSuffix(contents, []byte{'\r', '\n'})
	} else {
		contents = bytes.TrimSuffix(contents, []byte{'\n'})
	}
	if len(contents) < minimum || len(contents) > maximum {
		return "", errors.New("secret token length is outside the allowed range")
	}
	for _, character := range contents {
		if character < '!' || character > '~' {
			return "", errors.New("secret token must be printable ASCII without whitespace")
		}
	}
	return string(contents), nil
}

func loadAndCompile(options compileOptions) (tenantconfig.CompiledBundle, error) {
	sourceContents, err := os.ReadFile(options.sourcePath)
	if err != nil {
		return tenantconfig.CompiledBundle{}, fmt.Errorf("read policy source: %w", err)
	}
	var source tenantconfig.Source
	if err := strictjson.Decode(sourceContents, &source); err != nil {
		return tenantconfig.CompiledBundle{}, fmt.Errorf("decode policy source: %w", err)
	}
	bindingContents, err := os.ReadFile(options.bindingPath)
	if err != nil {
		return tenantconfig.CompiledBundle{}, fmt.Errorf("read environment binding: %w", err)
	}
	var binding tenantconfig.EnvironmentBinding
	if err := strictjson.Decode(bindingContents, &binding); err != nil {
		return tenantconfig.CompiledBundle{}, fmt.Errorf("decode environment binding: %w", err)
	}
	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		return tenantconfig.CompiledBundle{}, fmt.Errorf("compile tenant configuration: %w", err)
	}
	return compiled, nil
}

func observeLocal(root string, compiled tenantconfig.CompiledBundle) (tenantconfig.ObservedState, error) {
	observed := tenantconfig.ObservedState{LocalArtifacts: make(map[string]string)}
	for _, artifact := range compiled.Artifacts {
		path, err := safeArtifactPath(root, artifact.Path)
		if err != nil {
			return tenantconfig.ObservedState{}, err
		}
		contents, exists, err := readRegularFile(path)
		if err != nil {
			return tenantconfig.ObservedState{}, fmt.Errorf("observe local artifact %q: %w", artifact.Path, err)
		}
		if exists {
			observed.LocalArtifacts[artifact.Path] = sha256Hex(contents)
		}
	}
	return observed, nil
}

func marshalDocument(value any) ([]byte, error) {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(contents, '\n'), nil
}

func writeJSON(writer io.Writer, value any) error {
	contents, err := marshalDocument(value)
	if err != nil {
		return err
	}
	_, err = writer.Write(contents)
	return err
}

func sha256Hex(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}
