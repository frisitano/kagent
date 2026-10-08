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

func TestRevisionDigestIncludesDataVolume(t *testing.T) {
	revision := &Revision{Namespace: "agents", AgentTemplateName: "helper", HarnessName: "kagent"}
	durable, err := revision.Digest()
	require.NoError(t, err)
	require.Equal(t, "3edf8e1756778ce192e3c834e6ebd8e2421dc23e9d64ade7d6ee3c6d6897cd6d", durable.String(), "no data volume must preserve existing revisions")

	revision.DataVolume = &DataVolume{StorageClassName: "agent-data", Capacity: "20Gi"}
	external, err := revision.Digest()
	require.NoError(t, err)
	require.NotEqual(t, durable, external, "moving /data to an external volume must create a new revision")

	revision.DataVolume = &DataVolume{StorageClassName: "agent-data", Capacity: "40Gi"}
	resized, err := revision.Digest()
	require.NoError(t, err)
	require.NotEqual(t, external, resized)
}

func TestDataVolumeFromAnnotations(t *testing.T) {
	for _, tt := range []struct {
		name        string
		annotations map[string]string
		want        *DataVolume
		wantErr     bool
	}{
		{name: "unset keeps the DurableDir"},
		{name: "unrelated annotations", annotations: map[string]string{"other": "x"}},
		{name: "both", annotations: map[string]string{DataVolumeStorageClassAnnotation: "agent-data", DataVolumeCapacityAnnotation: "20Gi"}, want: &DataVolume{StorageClassName: "agent-data", Capacity: "20Gi"}},
		{name: "canonical capacity", annotations: map[string]string{DataVolumeStorageClassAnnotation: "agent-data", DataVolumeCapacityAnnotation: "20480Mi"}, want: &DataVolume{StorageClassName: "agent-data", Capacity: "20Gi"}},
		{name: "class only", annotations: map[string]string{DataVolumeStorageClassAnnotation: "agent-data"}, wantErr: true},
		{name: "capacity only", annotations: map[string]string{DataVolumeCapacityAnnotation: "20Gi"}, wantErr: true},
		{name: "bad class", annotations: map[string]string{DataVolumeStorageClassAnnotation: "Agent_Data", DataVolumeCapacityAnnotation: "20Gi"}, wantErr: true},
		{name: "bad capacity", annotations: map[string]string{DataVolumeStorageClassAnnotation: "agent-data", DataVolumeCapacityAnnotation: "twenty"}, wantErr: true},
		{name: "zero capacity", annotations: map[string]string{DataVolumeStorageClassAnnotation: "agent-data", DataVolumeCapacityAnnotation: "0"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dataVolumeFromAnnotations(tt.annotations)
			if tt.wantErr {
				var invalid *ValidationError
				require.ErrorAs(t, err, &invalid)
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
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
