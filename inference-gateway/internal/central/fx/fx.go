// Package fx fetches the ECB euro reference rates into central's FX
// observations (DES-HOR-661-04). Rates are locked per billing period by the
// store; no request ever looks up FX remotely.
package fx

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"time"
)

// Interval is how often the feed is fetched.
const Interval = time.Hour

// maxBody bounds the feed size.
const maxBody = 1 << 20

// Observation is one ECB reference rate: USD per 1 EUR on a date.
type Observation struct {
	Date      time.Time
	USDPerEUR *big.Rat
}

// Recorder stores observations.
type Recorder interface {
	RecordObservation(ctx context.Context, date time.Time, usdPerEUR *big.Rat) error
}

// HTTPDoer is the HTTP client.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// envelope is an ECB eurofxref document (daily or 90-day history).
type envelope struct {
	Cubes []struct {
		Time  string `xml:"time,attr"`
		Rates []struct {
			Currency string `xml:"currency,attr"`
			Rate     string `xml:"rate,attr"`
		} `xml:"Cube"`
	} `xml:"Cube>Cube"`
}

// Parse extracts the USD observations from an ECB reference-rate document.
func Parse(r io.Reader) ([]Observation, error) {
	var env envelope
	if err := xml.NewDecoder(io.LimitReader(r, maxBody)).Decode(&env); err != nil {
		return nil, fmt.Errorf("fx: parse ECB feed: %w", err)
	}
	var out []Observation
	for _, day := range env.Cubes {
		date, err := time.Parse(time.DateOnly, day.Time)
		if err != nil {
			return nil, fmt.Errorf("fx: bad ECB date %q", day.Time)
		}
		for _, rate := range day.Rates {
			if rate.Currency != "USD" {
				continue
			}
			r, ok := new(big.Rat).SetString(rate.Rate)
			if !ok || r.Sign() <= 0 {
				return nil, fmt.Errorf("fx: bad ECB USD rate %q", rate.Rate)
			}
			out = append(out, Observation{Date: date, USDPerEUR: r})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("fx: the ECB feed has no USD rate")
	}
	return out, nil
}

// Fetcher fetches the feed into a Recorder.
type Fetcher struct {
	URL      string
	Client   HTTPDoer
	Recorder Recorder
	Logger   *slog.Logger
}

// Fetch fetches and records the feed once.
func (f *Fetcher) Fetch(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return err
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return fmt.Errorf("fx: fetch ECB feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fx: ECB feed returned %d", resp.StatusCode)
	}
	observations, err := Parse(resp.Body)
	if err != nil {
		return err
	}
	for _, o := range observations {
		if err := f.Recorder.RecordObservation(ctx, o.Date, o.USDPerEUR); err != nil {
			return fmt.Errorf("fx: record observation: %w", err)
		}
	}
	return nil
}

// Run fetches now and then every Interval until ctx is done.
func (f *Fetcher) Run(ctx context.Context) {
	for {
		if err := f.Fetch(ctx); err != nil && ctx.Err() == nil {
			f.Logger.Warn("ECB reference rate fetch failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(Interval):
		}
	}
}
