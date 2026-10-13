package e2e_test

import (
	"strings"
	"testing"
)

func TestCleanDatabaseQueryOutputDropsKubectlStreamWarning(t *testing.T) {
	output := "E0816 13:00:19.754266   42934 websocket.go:296] Unknown stream id 1, discarding message\n10005\n"
	if cleaned := cleanDatabaseQueryOutput(output); cleaned != "10005" {
		t.Fatalf("cleaned database output=%q want 10005", cleaned)
	}
}

func TestReadSSEEvents(t *testing.T) {
	stream := strings.NewReader(`: heartbeat

id: 4
event: work_item_created
data: {"cursor":4,"id":"event-4","workItemId":"item-1","code":"work_item_created","params":{},"createdAt":"2026-08-15T00:00:00Z"}

id: 7
event: feedback_saved
data: {"cursor":7,"id":"event-7","workItemId":"item-1","code":"feedback_saved","params":{},"createdAt":"2026-08-15T00:00:01Z"}

`)
	events, err := readSSEEvents(stream, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Cursor != 4 || events[1].Cursor != 7 {
		t.Fatalf("events=%+v", events)
	}
}

func TestReadSSEEventsRejectsMismatchedID(t *testing.T) {
	_, err := readSSEEvents(strings.NewReader("id: 9\ndata: {\"cursor\":8}\n\n"), 1)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error=%v", err)
	}
}

func TestSafeResponseRedactsCredentialFields(t *testing.T) {
	response := safeResponse([]byte(`{"access_token":"jwt-secret","fullKey":"api-secret","nested":{"password":"db-secret"},"state":"ok"}`))
	for _, secret := range []string{"jwt-secret", "api-secret", "db-secret"} {
		if strings.Contains(response, secret) {
			t.Fatalf("safe response leaked %q: %s", secret, response)
		}
	}
	if !strings.Contains(response, `"state":"ok"`) {
		t.Fatalf("safe response removed non-secret evidence: %s", response)
	}
}

func TestValidateCustomerSafeJSON(t *testing.T) {
	if err := validateCustomerSafeJSON([]byte(`{"source":{"title":"Safe"},"state":"done"}`), privateSourceMarker); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"prompt":        `{"prompt":"hidden"}`,
		"worker":        `{"workerId":"worker-1"}`,
		"trace":         `{"rawToolTrace":[]}`,
		"private-value": `{"source":{"title":"` + privateSourceMarker + `"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCustomerSafeJSON([]byte(body), privateSourceMarker); err == nil {
				t.Fatalf("unsafe body accepted: %s", body)
			}
		})
	}
}
