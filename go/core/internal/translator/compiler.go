package translator

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"regexp"
	"strings"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"istio.io/istio/pkg/kube/krt"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Compiler resolves public API objects into a complete, immutable runtime
// revision. It owns the v2 translation boundary rather than delegating to an
// earlier API translator.
type Compiler struct {
	ctx              krt.HandlerContext
	collections      Collections
	harnessCompilers map[HarnessType]HarnessCompiler
}

// HarnessType identifies the runtime selected by a Harness.
type HarnessType string

// Supported Harness runtime types.
const (
	HarnessTypeKagent HarnessType = "kagent"
	HarnessTypeCodex  HarnessType = "codex"
	HarnessTypeClaude HarnessType = "claude"
	HarnessTypeBYO    HarnessType = "byo"
)

// HarnessCompiler converts resolved, harness-neutral inputs into one runtime
// revision and its user-facing diagnostics.
type HarnessCompiler interface {
	Compile(context.Context, *HarnessInput) (*CompileResult, error)
}

// ResolvedTree is the validated AgentTemplate topology for one Harness.
type ResolvedTree struct {
	Harness *v1alpha3.Harness
	Root    *ResolvedAgent
}

// ResolvedAgent is one template and its validated Shared children.
type ResolvedAgent struct {
	Template *v1alpha3.AgentTemplate
	Shared   []ResolvedAgentBinding
}

// ResolvedAgentBinding preserves the parent-specific identity of a Shared child.
type ResolvedAgentBinding struct {
	Name        string
	Description string
	Agent       *ResolvedAgent
}

// HarnessInput contains the Kubernetes inputs needed by a harness compiler.
type HarnessInput struct {
	Harness      *v1alpha3.Harness
	Root         *AgentInput
	OutputSchema *ResolvedOutputSchema
}

// AgentInput contains resolved Kubernetes inputs for one agent.
type AgentInput struct {
	Template            *v1alpha3.AgentTemplate
	ResolvedModelConfig *ResolvedModelConfig
	Instruction         string
	MCPTools            []ResolvedMCPTool
	Shared              []AgentInputBinding
}

// ResolvedMCPTool pairs an exact tool allowlist with its resolved server.
type ResolvedMCPTool struct {
	Binding v1alpha3.MCPToolBinding
	Server  *v1alpha3.RemoteMCPServer
}

// AgentInputBinding preserves the parent-specific identity of a compiled child.
type AgentInputBinding struct {
	Name        string
	Description string
	Agent       *AgentInput
}

// NewCompiler constructs the v2 runtime compiler.
func NewCompiler(ctx krt.HandlerContext, collections Collections, harnessCompilers map[HarnessType]HarnessCompiler) *Compiler {
	return &Compiler{ctx: ctx, collections: collections, harnessCompilers: maps.Clone(harnessCompilers)}
}

// CompileAgentTemplate resolves an API v2 attachment into an immutable runtime
// revision and user-facing diagnostics. Nothing below this boundary needs to
// read the public API objects.
func (c *Compiler) CompileAgentTemplate(ctx context.Context, harness *v1alpha3.Harness, template *v1alpha3.AgentTemplate) (*CompileResult, error) {
	harnessCompiler := c.harnessCompilers[harnessType(harness)]
	if harnessCompiler == nil {
		return nil, NewValidationError("Harness runtime is not supported by any compiler")
	}
	tree, err := c.resolveTree(ctx, harness, template)
	if err != nil {
		return nil, err
	}
	input, err := c.buildInputs(ctx, tree)
	if err != nil {
		return nil, err
	}
	result, err := harnessCompiler.Compile(ctx, input)
	if err != nil {
		return nil, err
	}
	workerKey := types.NamespacedName{Namespace: harness.Namespace, Name: harness.Spec.Substrate.WorkerPoolRef.Name}
	workerPool := krt.FetchOne(c.ctx, c.collections.WorkerPools, krt.FilterObjectName(workerKey))
	if workerPool == nil {
		return nil, &WorkerPoolNotFoundError{WorkerPool: workerKey}
	}
	result.SandboxClass = (*workerPool).Spec.SandboxClass
	volumes, err := harnessVolumes(harness.Annotations)
	if err != nil {
		return nil, err
	}
	result.Volumes = volumes
	return result, nil
}

func harnessType(harness *v1alpha3.Harness) HarnessType {
	switch {
	case harness.Spec.Kagent != nil:
		return HarnessTypeKagent
	case harness.Spec.Codex != nil:
		return HarnessTypeCodex
	case harness.Spec.Claude != nil:
		return HarnessTypeClaude
	case harness.Spec.BYO != nil:
		return HarnessTypeBYO
	default:
		return ""
	}
}

func (c *Compiler) resolveTree(ctx context.Context, harness *v1alpha3.Harness, root *v1alpha3.AgentTemplate) (*ResolvedTree, error) {
	selector, err := harnessSelector(harness)
	if err != nil {
		return nil, err
	}
	seen, path, names := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	var resolve func(*v1alpha3.AgentTemplate, bool) (*ResolvedAgent, error)
	resolve = func(template *v1alpha3.AgentTemplate, child bool) (*ResolvedAgent, error) {
		if template.Namespace != harness.Namespace {
			return nil, NewValidationError("Harness and AgentTemplate must be in the same namespace")
		}
		if !selector.Matches(labels.Set(template.Labels)) {
			return nil, NewValidationError("AgentTemplate %q is not admitted by Harness %q", template.Name, harness.Name)
		}
		if _, ok := path[template.Name]; ok {
			return nil, NewValidationError("AgentTemplate tool cycle includes %q", template.Name)
		}
		if _, ok := seen[template.Name]; ok {
			return nil, NewValidationError("AgentTemplate %q is referenced more than once in the Shared tree", template.Name)
		}
		seen[template.Name], path[template.Name] = struct{}{}, struct{}{}
		defer delete(path, template.Name)

		resolved := &ResolvedAgent{Template: template.DeepCopy()}
		for _, tool := range template.Spec.Tools {
			if tool.Agent == nil {
				continue
			}
			binding := tool.Agent
			if binding.Isolation == v1alpha3.AgentToolIsolationDedicated {
				return nil, NewValidationError("Dedicated AgentTemplate tools are not supported yet")
			}
			if _, ok := names[binding.Name]; ok {
				return nil, NewValidationError("duplicate Shared AgentTemplate binding name %q", binding.Name)
			}
			names[binding.Name] = struct{}{}
			key := types.NamespacedName{Namespace: template.Namespace, Name: binding.TemplateRef.Name}
			childTemplate := krt.FetchOne(c.ctx, c.collections.AgentTemplates, krt.FilterObjectName(key))
			if childTemplate == nil {
				return nil, fmt.Errorf("resolve AgentTemplate %q: not found", binding.TemplateRef.Name)
			}
			agent, err := resolve(*childTemplate, true)
			if err != nil {
				return nil, err
			}
			if child {
				return nil, NewValidationError("consecutive Shared AgentTemplate tools exceed the kagent runtime boundary")
			}
			resolved.Shared = append(resolved.Shared, ResolvedAgentBinding{Name: binding.Name, Description: binding.Description, Agent: agent})
		}
		return resolved, nil
	}

	resolved, err := resolve(root, false)
	if err != nil {
		return nil, err
	}
	return &ResolvedTree{Harness: harness.DeepCopy(), Root: resolved}, nil
}

func harnessSelector(harness *v1alpha3.Harness) (labels.Selector, error) {
	if harness.Spec.AllowedAgentTemplates == nil {
		return labels.Nothing(), NewValidationError("Harness %q admits no AgentTemplates", harness.Name)
	}
	selector, err := metav1.LabelSelectorAsSelector(&harness.Spec.AllowedAgentTemplates.Selector)
	if err != nil {
		return nil, NewValidationError("Harness %q has an invalid AgentTemplate selector: %v", harness.Name, err)
	}
	return selector, nil
}

func (c *Compiler) buildInputs(ctx context.Context, tree *ResolvedTree) (*HarnessInput, error) {
	outputSchema, err := c.resolveOutputSchema(ctx, tree.Root.Template)
	if err != nil {
		return nil, err
	}
	if outputSchema != nil && harnessType(tree.Harness) != HarnessTypeKagent {
		return nil, NewValidationError("Harness %q does not support structured output", tree.Harness.Name)
	}
	var build func(*ResolvedAgent) (*AgentInput, error)
	build = func(agent *ResolvedAgent) (*AgentInput, error) {
		template := agent.Template
		instruction, err := c.resolveAgentTemplatePrompt(ctx, template)
		if err != nil {
			return nil, err
		}
		input := &AgentInput{Template: template, Instruction: instruction}
		if template.Spec.ModelConfig != nil {
			input.ResolvedModelConfig = krt.FetchOne(c.ctx, c.collections.ResolvedModelConfigs, krt.FilterObjectName(types.NamespacedName{Namespace: template.Namespace, Name: template.Spec.ModelConfig.Name}))
			if input.ResolvedModelConfig == nil {
				return nil, fmt.Errorf("resolve ModelConfig %q: not found", template.Spec.ModelConfig.Name)
			}
			if failures := input.ResolvedModelConfig.SemanticFailures; len(failures) > 0 {
				return nil, NewValidationError("ModelConfig %q: %s", template.Spec.ModelConfig.Name, failures[0].Message)
			}
			if failures := input.ResolvedModelConfig.ReferenceFailures; len(failures) > 0 {
				return nil, fmt.Errorf("resolve ModelConfig %q: %s", template.Spec.ModelConfig.Name, failures[0].Message)
			}
		}
		toolNames := make([]string, 0)
		for _, tool := range template.Spec.Tools {
			if tool.MCP == nil {
				if tool.Agent == nil {
					return nil, NewValidationError("tool binding must select an MCP server or AgentTemplate")
				}
				continue
			}
			if tool.MCP.Server.Kind != "RemoteMCPServer" {
				return nil, NewValidationError("unsupported MCP server kind %q", tool.MCP.Server.Kind)
			}
			key := types.NamespacedName{Namespace: template.Namespace, Name: tool.MCP.Server.Name}
			server := krt.FetchOne(c.ctx, c.collections.RemoteMCPServers, krt.FilterObjectName(key))
			if server == nil {
				return nil, fmt.Errorf("resolve %s %q: not found", tool.MCP.Server.Kind, tool.MCP.Server.Name)
			}
			input.MCPTools = append(input.MCPTools, ResolvedMCPTool{Binding: *tool.MCP.DeepCopy(), Server: *server})
			toolNames = append(toolNames, tool.MCP.Tools...)
		}
		if template.Spec.PromptTemplate != nil {
			refs := make([]promptSourceRef, 0, len(template.Spec.PromptTemplate.DataSources))
			for _, source := range template.Spec.PromptTemplate.DataSources {
				refs = append(refs, promptSourceRef{Name: source.Name, Alias: source.Alias})
			}
			lookup, err := resolvePromptSourceRefs(c.ctx, c.collections.ConfigMaps, template.Namespace, refs)
			if err != nil {
				return nil, fmt.Errorf("resolve prompt sources: %w", err)
			}
			input.Instruction, err = executeSystemMessageTemplate(input.Instruction, lookup, PromptTemplateContext{
				AgentTemplateName: template.Name, AgentTemplateNamespace: template.Namespace,
				Description: template.Spec.Description, ToolNames: toolNames,
			})
			if err != nil {
				return nil, err
			}
		}
		for _, binding := range agent.Shared {
			child, err := build(binding.Agent)
			if err != nil {
				return nil, err
			}
			input.Shared = append(input.Shared, AgentInputBinding{Name: binding.Name, Description: binding.Description, Agent: child})
		}
		return input, nil
	}

	root, err := build(tree.Root)
	if err != nil {
		return nil, err
	}
	return &HarnessInput{Harness: tree.Harness, Root: root, OutputSchema: outputSchema}, nil
}

// Harness annotations that give an Actor external volumes, each created per Actor from a StorageClass
// whose provisioner has a Substrate CSIDriverConfig, attached with the Actor and never in its snapshots.
const (
	// DataVolumeStorageClassAnnotation and DataVolumeCapacityAnnotation, set together, put /data on an
	// external volume instead of a durable directory that Substrate archives on every suspend.
	DataVolumeStorageClassAnnotation = "kagent.dev/data-volume-storage-class"
	DataVolumeCapacityAnnotation     = "kagent.dev/data-volume-capacity"
	// ExtraVolumesAnnotation is a JSON list of further volumes, [{"name", "mountPath",
	// "storageClassName", "capacity"}]. A StorageClass that maps every Actor to one directory makes one
	// shared between Actors. {"name", "mountPath", "image"} mounts an image pinned by digest, read-only.
	ExtraVolumesAnnotation = "kagent.dev/extra-volumes"
)

// DataVolumeName is the volume mounted at DataVolumeMountPath.
const (
	DataVolumeName      = "data"
	DataVolumeMountPath = "/data"
)

var reservedVolumeNames = map[string]bool{DataVolumeName: true, "egress-trust": true, "actor-identity": true, "durable-state": true}

func harnessVolumes(annotations map[string]string) ([]Volume, error) {
	var volumes []Volume
	storageClass, hasClass := annotations[DataVolumeStorageClassAnnotation]
	capacity, hasCapacity := annotations[DataVolumeCapacityAnnotation]
	if hasClass != hasCapacity {
		return nil, NewValidationError("Harness annotations %s and %s must be set together", DataVolumeStorageClassAnnotation, DataVolumeCapacityAnnotation)
	}
	if hasClass {
		volume, err := validVolume(Volume{Name: DataVolumeName, MountPath: DataVolumeMountPath, StorageClassName: storageClass, Capacity: capacity})
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, volume)
	}
	raw, ok := annotations[ExtraVolumesAnnotation]
	if !ok {
		return volumes, nil
	}
	var extra []Volume
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&extra); err != nil {
		return nil, NewValidationError("Harness annotation %s: %v", ExtraVolumesAnnotation, err)
	}
	seen := map[string]bool{DataVolumeMountPath: true}
	names := map[string]bool{}
	for _, volume := range extra {
		if reservedVolumeNames[volume.Name] || names[volume.Name] {
			return nil, NewValidationError("Harness annotation %s: volume name %q is reserved or repeated", ExtraVolumesAnnotation, volume.Name)
		}
		if seen[volume.MountPath] || volume.MountPath == "/run/kagent" || strings.HasPrefix(volume.MountPath, "/run/kagent/") {
			return nil, NewValidationError("Harness annotation %s: mountPath %q is reserved or repeated", ExtraVolumesAnnotation, volume.MountPath)
		}
		volume, err := validVolume(volume)
		if err != nil {
			return nil, err
		}
		names[volume.Name], seen[volume.MountPath] = true, true
		volumes = append(volumes, volume)
	}
	return volumes, nil
}

// pinnedImage is an image reference with a sha256 digest, as Substrate's image volumes require.
var pinnedImage = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

// validVolume checks one volume and normalizes its capacity, so equal quantities digest alike.
func validVolume(volume Volume) (Volume, error) {
	if errs := validation.IsDNS1123Label(volume.Name); len(errs) != 0 {
		return Volume{}, NewValidationError("volume name %q: %s", volume.Name, strings.Join(errs, "; "))
	}
	if !path.IsAbs(volume.MountPath) || path.Clean(volume.MountPath) != volume.MountPath || volume.MountPath == "/" || strings.ContainsAny(volume.MountPath, ":") {
		return Volume{}, NewValidationError("volume %q: mountPath %q must be a clean absolute path", volume.Name, volume.MountPath)
	}
	if volume.Image != "" {
		if volume.StorageClassName != "" || volume.Capacity != "" {
			return Volume{}, NewValidationError("volume %q: an image volume has no storageClassName or capacity", volume.Name)
		}
		if !pinnedImage.MatchString(volume.Image) {
			return Volume{}, NewValidationError("volume %q: image %q must be pinned by digest (name@sha256:<64 hex>)", volume.Name, volume.Image)
		}
		return volume, nil
	}
	if errs := validation.IsDNS1123Subdomain(volume.StorageClassName); len(errs) != 0 {
		return Volume{}, NewValidationError("volume %q: storageClassName %q: %s", volume.Name, volume.StorageClassName, strings.Join(errs, "; "))
	}
	quantity, err := resource.ParseQuantity(volume.Capacity)
	if err != nil || quantity.Sign() <= 0 {
		return Volume{}, NewValidationError("volume %q: capacity %q must be a positive quantity", volume.Name, volume.Capacity)
	}
	volume.Capacity = quantity.String()
	return volume, nil
}
