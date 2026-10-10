package translator

import (
	"strings"
	"testing"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestRevisionDigestIncludesSandboxClass(t *testing.T) {
	revision := &Revision{Namespace: "agents", AgentTemplateName: "helper", HarnessName: "kagent"}
	original, err := revision.Digest()
	require.NoError(t, err)
	require.Equal(t, "3edf8e1756778ce192e3c834e6ebd8e2421dc23e9d64ade7d6ee3c6d6897cd6d", original.String())

	revision.SandboxClass = atev1alpha1.SandboxClassGvisor
	gvisor, err := revision.Digest()
	require.NoError(t, err)
	require.Equal(t, original, gvisor, "explicit gVisor must preserve the existing default revision")
	require.Equal(t, atev1alpha1.SandboxClassGvisor, revision.SandboxClass, "hashing must not mutate the revision")

	revision.SandboxClass = atev1alpha1.SandboxClassMicroVM
	microvm, err := revision.Digest()
	require.NoError(t, err)
	require.NotEqual(t, gvisor, microvm, "changing sandbox class must create a new immutable revision")
	repeated, err := revision.Digest()
	require.NoError(t, err)
	require.Equal(t, microvm, repeated)

	revision.SandboxClass = "unsupported"
	invalid, err := revision.Digest()
	require.EqualError(t, err, `unsupported sandbox class "unsupported"`)
	require.True(t, invalid.IsZero())
}

func TestRevisionDigestIncludesProvenance(t *testing.T) {
	revision := &Revision{Namespace: "agents", AgentTemplateName: "helper", HarnessName: "kagent", Provenance: []byte(`[{"kind":"ConfigMap","hash":"first"}]`)}
	first, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	revision.Provenance = []byte(`[{"kind":"ConfigMap","hash":"second"}]`)
	second, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("configuration change did not change runtime revision")
	}
	if len(first.Short()) != 12 || !strings.HasPrefix(first.String(), first.Short()) {
		t.Fatalf("short revision %q is not a prefix of %q", first.Short(), first.String())
	}
}

func TestRevisionDigestIncludesConfig(t *testing.T) {
	revision := &Revision{Namespace: "agents", AgentTemplateName: "helper", HarnessName: "claude", ConfigJSON: []byte(`{"version":5,"runtime_telemetry":{"capture_content":false}}`)}
	first, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	revision.ConfigJSON = []byte(`{"version":5,"runtime_telemetry":{"capture_content":true}}`)
	second, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("compiled configuration change did not change runtime revision")
	}
}

func TestCompilationWarningsDoNotAffectRevisionDigest(t *testing.T) {
	compilation := &CompileResult{Revision: Revision{Namespace: "agents", AgentTemplateName: "helper", HarnessName: "claude"}}
	first, err := compilation.Digest()
	if err != nil {
		t.Fatal(err)
	}
	compilation.Warnings = []string{"partial MCP selection is not enforced"}
	if len(compilation.Warnings) != 1 {
		t.Fatalf("warnings = %v, want one warning", compilation.Warnings)
	}
	second, err := compilation.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("non-behavioral warning changed runtime revision")
	}
}

func TestRevisionDigestIncludesCommand(t *testing.T) {
	revision := &Revision{Namespace: "agents", AgentTemplateName: "helper", HarnessName: "byo", Command: []string{"/agent"}}
	first, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	revision.Command = []string{"/other-agent"}
	second, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("command change did not change runtime revision")
	}
}

func TestRevisionDigestIncludesBinaryAgentCard(t *testing.T) {
	card := &a2apb.AgentCard{Name: "assistant"}
	revision := &Revision{AgentCard: card}
	first, err := revision.Digest()
	require.NoError(t, err)
	card.Name = "changed"
	second, err := revision.Digest()
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	card.ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 1000, protowire.BytesType), "future"))
	third, err := revision.Digest()
	require.NoError(t, err)
	require.NotEqual(t, second, third)
	data, err := proto.Marshal(card)
	require.NoError(t, err)
	revision.AgentCard = &a2apb.AgentCard{}
	require.NoError(t, proto.Unmarshal(data, revision.AgentCard))
	roundTrip, err := revision.Digest()
	require.NoError(t, err)
	require.Equal(t, third, roundTrip)
	revision.AgentCard.Name = string([]byte{0xff})
	_, err = revision.Digest()
	require.Error(t, err)
}

func TestRevisionDigestIncludesVolumesOnlyWhenSet(t *testing.T) {
	revision := &Revision{Namespace: "agents", AgentTemplateName: "helper", HarnessName: "kagent"}
	original, err := revision.Digest()
	require.NoError(t, err)
	require.Equal(t, "3edf8e1756778ce192e3c834e6ebd8e2421dc23e9d64ade7d6ee3c6d6897cd6d", original.String(), "a revision without volumes keeps its digest")

	revision.Volumes = []Volume{{Name: "data", MountPath: "/data", StorageClassName: "agent-data", Capacity: "20Gi"}}
	withData, err := revision.Digest()
	require.NoError(t, err)
	require.NotEqual(t, original, withData)
	revision.Volumes[0].Capacity = "30Gi"
	larger, err := revision.Digest()
	require.NoError(t, err)
	require.NotEqual(t, withData, larger)
}

func TestHarnessVolumes(t *testing.T) {
	for _, test := range []struct {
		name        string
		annotations map[string]string
		want        []Volume
		err         string
	}{
		{name: "none"},
		{name: "data", annotations: map[string]string{DataVolumeStorageClassAnnotation: "agent-data", DataVolumeCapacityAnnotation: "20480Mi"},
			want: []Volume{{Name: "data", MountPath: "/data", StorageClassName: "agent-data", Capacity: "20Gi"}}},
		{name: "data and a shared cache", annotations: map[string]string{
			DataVolumeStorageClassAnnotation: "agent-data", DataVolumeCapacityAnnotation: "20Gi",
			ExtraVolumesAnnotation: `[{"name":"cache","mountPath":"/cache","storageClassName":"shared-cache","capacity":"100Gi"}]`},
			want: []Volume{
				{Name: "data", MountPath: "/data", StorageClassName: "agent-data", Capacity: "20Gi"},
				{Name: "cache", MountPath: "/cache", StorageClassName: "shared-cache", Capacity: "100Gi"},
			}},
		{name: "unrelated annotations", annotations: map[string]string{"other": "x"}},
		{name: "a shared cache without a data volume", annotations: map[string]string{
			ExtraVolumesAnnotation: `[{"name":"cache","mountPath":"/cache","storageClassName":"shared-cache","capacity":"100Gi"}]`},
			want: []Volume{{Name: "cache", MountPath: "/cache", StorageClassName: "shared-cache", Capacity: "100Gi"}}},
		{name: "class without capacity", annotations: map[string]string{DataVolumeStorageClassAnnotation: "agent-data"}, err: "must be set together"},
		{name: "capacity without class", annotations: map[string]string{DataVolumeCapacityAnnotation: "20Gi"}, err: "must be set together"},
		{name: "zero capacity", annotations: map[string]string{DataVolumeStorageClassAnnotation: "agent-data", DataVolumeCapacityAnnotation: "0"}, err: "positive quantity"},
		{name: "extra not JSON", annotations: map[string]string{ExtraVolumesAnnotation: `cache=/cache`}, err: ExtraVolumesAnnotation},
		{name: "extra named durable-state", annotations: map[string]string{ExtraVolumesAnnotation: `[{"name":"durable-state","mountPath":"/cache","storageClassName":"c","capacity":"1Gi"}]`}, err: "reserved or repeated"},
		{name: "extra repeated path", annotations: map[string]string{ExtraVolumesAnnotation: `[{"name":"a","mountPath":"/a","storageClassName":"c","capacity":"1Gi"},{"name":"b","mountPath":"/a","storageClassName":"c","capacity":"1Gi"}]`}, err: "reserved or repeated"},
		{name: "extra relative path", annotations: map[string]string{ExtraVolumesAnnotation: `[{"name":"a","mountPath":"cache","storageClassName":"c","capacity":"1Gi"}]`}, err: "clean absolute path"},
		{name: "bad capacity", annotations: map[string]string{DataVolumeStorageClassAnnotation: "agent-data", DataVolumeCapacityAnnotation: "lots"}, err: "positive quantity"},
		{name: "bad class", annotations: map[string]string{DataVolumeStorageClassAnnotation: "Agent_Data", DataVolumeCapacityAnnotation: "1Gi"}, err: "storageClassName"},
		{name: "extra named data", annotations: map[string]string{ExtraVolumesAnnotation: `[{"name":"data","mountPath":"/cache","storageClassName":"c","capacity":"1Gi"}]`}, err: "reserved or repeated"},
		{name: "extra at /data", annotations: map[string]string{ExtraVolumesAnnotation: `[{"name":"cache","mountPath":"/data","storageClassName":"c","capacity":"1Gi"}]`}, err: "reserved or repeated"},
		{name: "extra under /run/kagent", annotations: map[string]string{ExtraVolumesAnnotation: `[{"name":"cache","mountPath":"/run/kagent/x","storageClassName":"c","capacity":"1Gi"}]`}, err: "reserved or repeated"},
		{name: "extra repeated", annotations: map[string]string{ExtraVolumesAnnotation: `[{"name":"a","mountPath":"/a","storageClassName":"c","capacity":"1Gi"},{"name":"a","mountPath":"/b","storageClassName":"c","capacity":"1Gi"}]`}, err: "reserved or repeated"},
		{name: "extra unclean path", annotations: map[string]string{ExtraVolumesAnnotation: `[{"name":"a","mountPath":"/a/../b","storageClassName":"c","capacity":"1Gi"}]`}, err: "clean absolute path"},
		{name: "a store image", annotations: map[string]string{
			ExtraVolumesAnnotation: `[{"name":"nix-shared","mountPath":"/nix/shared","image":"registry.example/store@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`},
			want: []Volume{{Name: "nix-shared", MountPath: "/nix/shared", Image: "registry.example/store@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}},
		{name: "an image by tag", annotations: map[string]string{ExtraVolumesAnnotation: `[{"name":"a","mountPath":"/a","image":"registry.example/store:latest"}]`}, err: "pinned by digest"},
		{name: "an image with a capacity", annotations: map[string]string{ExtraVolumesAnnotation: `[{"name":"a","mountPath":"/a","image":"registry.example/store@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","capacity":"1Gi"}]`}, err: "no storageClassName or capacity"},
		{name: "an image named data", annotations: map[string]string{ExtraVolumesAnnotation: `[{"name":"data","mountPath":"/x","image":"registry.example/store@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`}, err: "reserved or repeated"},
		{name: "extra unknown field", annotations: map[string]string{ExtraVolumesAnnotation: `[{"name":"a","mountPath":"/a","storageClassName":"c","capacity":"1Gi","readOnly":true}]`}, err: "unknown field"},
	} {
		t.Run(test.name, func(t *testing.T) {
			volumes, err := harnessVolumes(test.annotations)
			if test.err != "" {
				require.ErrorContains(t, err, test.err)
				var invalid *ValidationError
				require.ErrorAs(t, err, &invalid, "a bad annotation must surface on the AgentTemplate status")
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, volumes)
		})
	}
}
