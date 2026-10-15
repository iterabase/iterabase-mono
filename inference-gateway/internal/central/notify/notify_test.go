package notify

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/store"
)

type fakeSNS struct {
	inputs []*sns.PublishInput
	err    error
}

func (f *fakeSNS) Publish(_ context.Context, in *sns.PublishInput, _ ...func(*sns.Options)) (*sns.PublishOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.inputs = append(f.inputs, in)
	return &sns.PublishOutput{}, nil
}

type fakeAlertStore struct {
	pending []store.Alert
	marked  []store.Alert
}

func (f *fakeAlertStore) PendingAlerts(context.Context) ([]store.Alert, error) { return f.pending, nil }
func (f *fakeAlertStore) MarkAlertNotified(_ context.Context, a store.Alert) error {
	f.marked = append(f.marked, a)
	return nil
}

func TestAlerts_FlushPublishesAndMarks(t *testing.T) {
	alert := store.Alert{InstallationID: "acme", Threshold: 80, PeriodStart: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		UsedCredits: "4000.000000", AllowanceCredits: "5000.000000"}
	client := &fakeSNS{}
	st := &fakeAlertStore{pending: []store.Alert{alert}}
	a := &Alerts{Store: st, Publisher: &SNSPublisher{Client: client, TopicARN: "arn:topic"}, Logger: slog.Default()}

	client.err = errors.New("throttled")
	assert.Error(t, a.Flush(context.Background()))
	assert.Empty(t, st.marked, "an unpublished alert stays pending")

	client.err = nil
	require.NoError(t, a.Flush(context.Background()))
	require.Len(t, client.inputs, 1)
	assert.Equal(t, "Iterabase credits: acme reached 80%", *client.inputs[0].Subject)
	assert.Contains(t, *client.inputs[0].Message, `"used_credits":"4000.000000"`)
	assert.Equal(t, []store.Alert{alert}, st.marked)
}

func TestSNSPublisher_TruncatesSubject(t *testing.T) {
	client := &fakeSNS{}
	require.NoError(t, (&SNSPublisher{Client: client, TopicARN: "arn"}).Publish(context.Background(), strings.Repeat("x", 150), "m"))
	assert.Len(t, *client.inputs[0].Subject, 100)
}
