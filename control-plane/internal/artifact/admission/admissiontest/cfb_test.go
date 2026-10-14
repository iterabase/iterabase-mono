package admissiontest

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/richardlehane/mscfb"
	"github.com/stretchr/testify/require"
)

func TestCFBWriterRoundTripsThroughMSCFB(t *testing.T) {
	big := bytes.Repeat([]byte("0123456789abcdef"), 600)
	data := CFB(
		CFBStream("small", []byte("hello mini stream")),
		CFBStream("big", big),
		CFBStorage("folder", CFBStream("inner", []byte("nested")), CFBStream("empty", nil)),
	)
	reader, err := mscfb.New(bytes.NewReader(data))
	require.NoError(t, err)
	got := map[string]string{}
	for _, f := range reader.File {
		if f.FileInfo().IsDir() {
			continue
		}
		content, err := io.ReadAll(f)
		require.NoError(t, err)
		got[fmt.Sprint(append(append([]string{}, f.Path...), f.Name))] = string(content)
	}
	require.Equal(t, map[string]string{
		"[small]":        "hello mini stream",
		"[big]":          string(big),
		"[folder inner]": "nested",
		"[folder empty]": "",
	}, got)
}

func TestBuildersAreDeterministic(t *testing.T) {
	first, second := Corpus(), Corpus()
	require.Len(t, second, len(first))
	for i := range first {
		require.Equal(t, first[i].Data, second[i].Data, first[i].Name)
	}
}

func TestWithMarker(t *testing.T) {
	data := []byte("abc")
	marked := WithMarker(data)
	require.Equal(t, "abc"+MalwareMarker, string(marked))
	require.Equal(t, "abc", string(data))
}
