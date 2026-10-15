package fx

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ecbDaily = `<?xml version="1.0" encoding="UTF-8"?>
<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref">
	<gesmes:subject>Reference rates</gesmes:subject>
	<gesmes:Sender><gesmes:name>European Central Bank</gesmes:name></gesmes:Sender>
	<Cube>
		<Cube time='2026-10-13'>
			<Cube currency='USD' rate='1.1403'/>
			<Cube currency='JPY' rate='170.12'/>
		</Cube>
	</Cube>
</gesmes:Envelope>`

// ecbHistory is the 90-day feed shape: several days, newest first.
const ecbHistory = `<?xml version="1.0" encoding="UTF-8"?>
<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref">
	<Cube>
		<Cube time='2026-10-13'><Cube currency='USD' rate='1.1403'/></Cube>
		<Cube time='2026-09-30'><Cube currency='USD' rate='1.1350'/></Cube>
		<Cube time='2026-09-29'><Cube currency='USD' rate='1.1300'/></Cube>
	</Cube>
</gesmes:Envelope>`

type recorded struct {
	date time.Time
	rate *big.Rat
}

type fakeRecorder struct{ got []recorded }

func (f *fakeRecorder) RecordObservation(_ context.Context, date time.Time, r *big.Rat) error {
	f.got = append(f.got, recorded{date, r})
	return nil
}

type fakeClient struct {
	status int
	body   string
}

func (f fakeClient) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader(f.body))}, nil
}

func TestParse(t *testing.T) {
	obs, err := Parse(strings.NewReader(ecbDaily))
	require.NoError(t, err)
	require.Len(t, obs, 1)
	assert.Equal(t, time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), obs[0].Date)
	assert.Equal(t, "1.1403", obs[0].USDPerEUR.FloatString(4))

	_, err = Parse(strings.NewReader(strings.Replace(ecbDaily, "currency='USD'", "currency='GBP'", 1)))
	assert.Error(t, err, "no USD rate")
	_, err = Parse(strings.NewReader(strings.Replace(ecbDaily, "1.1403", "-1", 1)))
	assert.Error(t, err)
	_, err = Parse(strings.NewReader("not xml"))
	assert.Error(t, err)
}

func TestFetch(t *testing.T) {
	rec := &fakeRecorder{}
	f := &Fetcher{URL: "https://ecb.test/daily.xml", Client: fakeClient{200, ecbDaily}, Recorder: rec, Logger: slog.Default()}
	require.NoError(t, f.Fetch(context.Background()))
	require.Len(t, rec.got, 1)

	rec = &fakeRecorder{}
	f.Recorder, f.Client = rec, fakeClient{200, ecbHistory}
	require.NoError(t, f.Fetch(context.Background()))
	require.Len(t, rec.got, 3, "every day of the 90-day feed is recorded, so a fresh database can lock earlier periods")
	assert.Equal(t, time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), rec.got[1].date)

	f.Client = fakeClient{503, ""}
	assert.Error(t, f.Fetch(context.Background()))
}
