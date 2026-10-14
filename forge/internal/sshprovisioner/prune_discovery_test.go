package sshprovisioner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDiscovered(t *testing.T) {
	raw := pruneMarker + "pods\n" +
		pruneMarker + "configmaps\n" +
		`{"kind":"List","items":[{"kind":"ConfigMap","metadata":{"name":"a","namespace":"x","labels":{"iterabase.com/owner":"overlay"}}}]}` + "\n" +
		pruneMarker + "namespaces\n" +
		`{"kind":"Namespace","metadata":{"name":"probe"}}` + "\n"
	listings, err := parseDiscovered(raw)
	require.NoError(t, err)
	require.Len(t, listings, 2, "resources without matches are dropped")
	assert.Equal(t, "configmaps", listings[0].resource)
	assert.Equal(t, "a", listings[0].objects[0].Metadata.Name)
	assert.Equal(t, "namespaces", listings[1].resource)
	assert.Equal(t, "Namespace/probe", listings[1].objects[0].ref())
}

func TestParseDiscoveredRejectsMalformedListing(t *testing.T) {
	_, err := parseDiscovered(pruneMarker + "configmaps\n{not json\n")
	require.ErrorContains(t, err, "discover owned configmaps")
}
