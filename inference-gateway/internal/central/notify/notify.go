// Package notify publishes credit alerts and operations alerts to Iterabase
// through the SNS billing-alerts topic (DES-HOR-661-06). Messages are
// payload-free.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"

	"github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/central/store"
)

// Publisher sends one message.
type Publisher interface {
	Publish(ctx context.Context, subject, message string) error
}

// SNSAPI is the subset of the SNS client used.
type SNSAPI interface {
	Publish(ctx context.Context, in *sns.PublishInput, opts ...func(*sns.Options)) (*sns.PublishOutput, error)
}

// SNSPublisher publishes to one topic.
type SNSPublisher struct {
	Client   SNSAPI
	TopicARN string
}

// Publish publishes a message. SNS subjects are limited to 100 characters.
func (p *SNSPublisher) Publish(ctx context.Context, subject, message string) error {
	if len(subject) > 100 {
		subject = subject[:100]
	}
	_, err := p.Client.Publish(ctx, &sns.PublishInput{
		TopicArn: aws.String(p.TopicARN), Subject: aws.String(subject), Message: aws.String(message),
	})
	if err != nil {
		return fmt.Errorf("notify: SNS publish: %w", err)
	}
	return nil
}

// LogPublisher logs instead of publishing (no topic configured).
type LogPublisher struct{ Logger *slog.Logger }

// Publish logs the message.
func (p LogPublisher) Publish(_ context.Context, subject, message string) error {
	p.Logger.Warn("alert (no SNS topic configured)", "subject", subject, "message", message)
	return nil
}

// AlertMessage renders a credit alert.
func AlertMessage(a store.Alert) (subject, message string) {
	body, _ := json.Marshal(map[string]any{
		"type": "credit_alert", "installation_id": a.InstallationID, "threshold": a.Threshold,
		"period_start": a.PeriodStart, "crossed_at": a.CrossedAt,
		"used_credits": a.UsedCredits, "allowance_credits": a.AllowanceCredits,
	})
	return fmt.Sprintf("Iterabase credits: %s reached %d%%", a.InstallationID, a.Threshold), string(body)
}

// AlertStore lists and marks unpublished alerts.
type AlertStore interface {
	PendingAlerts(ctx context.Context) ([]store.Alert, error)
	MarkAlertNotified(ctx context.Context, a store.Alert) error
}

// Alerts publishes credit alerts, immediately after the usage write that
// crossed them and from a retry loop for any that failed.
type Alerts struct {
	Store     AlertStore
	Publisher Publisher
	Logger    *slog.Logger
}

// Publish publishes one alert and marks it notified.
func (a *Alerts) Publish(ctx context.Context, alert store.Alert) error {
	subject, message := AlertMessage(alert)
	if err := a.Publisher.Publish(ctx, subject, message); err != nil {
		return err
	}
	return a.Store.MarkAlertNotified(ctx, alert)
}

// Flush publishes every pending alert once.
func (a *Alerts) Flush(ctx context.Context) error {
	pending, err := a.Store.PendingAlerts(ctx)
	if err != nil {
		return err
	}
	for _, alert := range pending {
		if err := a.Publish(ctx, alert); err != nil {
			return err
		}
	}
	return nil
}

// Run flushes pending alerts every interval until ctx is done.
func (a *Alerts) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.Flush(ctx); err != nil && ctx.Err() == nil {
				a.Logger.Warn("credit alert publish failed; will retry", "error", err)
			}
		}
	}
}
