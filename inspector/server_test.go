package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	sqlstore "github.com/go-estoria/estoria-contrib/sqlite/eventstore"
	sqlstrategy "github.com/go-estoria/estoria-contrib/sqlite/eventstore/strategy"
	"github.com/go-estoria/estoria/eventstore"
	"github.com/go-estoria/estoria/typeid"
	"github.com/gofrs/uuid/v5"
)

func TestParseStreamID(t *testing.T) {
	t.Parallel()

	t.Run("valid stream ID", func(t *testing.T) {
		t.Parallel()
		id, err := parseStreamID("board_e5701a1a-b0a2-4d00-8000-000000000001")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id.Type != "board" {
			t.Errorf("type = %q, want %q", id.Type, "board")
		}
		if got := id.UUID.String(); got != "e5701a1a-b0a2-4d00-8000-000000000001" {
			t.Errorf("uuid = %q, want %q", got, "e5701a1a-b0a2-4d00-8000-000000000001")
		}
	})

	t.Run("snapshot stream ID", func(t *testing.T) {
		t.Parallel()
		id, err := parseStreamID("boardsnapshot_e5701a1a-b0a2-4d00-8000-000000000001")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id.Type != "boardsnapshot" {
			t.Errorf("type = %q, want %q", id.Type, "boardsnapshot")
		}
	})

	for name, input := range map[string]string{
		"empty":                   "",
		"no separator":            "boarde5701a1a",
		"empty type":              "_e5701a1a-b0a2-4d00-8000-000000000001",
		"not a UUID":              "board_not-a-uuid",
		"underscore in type name": "foo_bar_e5701a1a-b0a2-4d00-8000-000000000001", // first-underscore split makes "bar_…" the UUID part
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseStreamID(input); err == nil {
				t.Errorf("parseStreamID(%q) succeeded, want error", input)
			}
		})
	}
}

func TestEncodeEventData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		data         []byte
		wantData     string
		wantEncoding string
	}{
		{"JSON object", []byte(`{"a":1}`), `{"a":1}`, "json"},
		{"JSON array", []byte(`[1,2,3]`), `[1,2,3]`, "json"},
		{"JSON scalar", []byte(`42`), `42`, "json"},
		{"binary", []byte{0xff, 0xfe, 0x01}, `"` + base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe, 0x01}) + `"`, "base64"},
		{"non-JSON text", []byte("hello, world"), `"` + base64.StdEncoding.EncodeToString([]byte("hello, world")) + `"`, "base64"},
		{"empty", nil, "null", "empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, encoding := encodeEventData(tt.data)
			if string(data) != tt.wantData {
				t.Errorf("data = %s, want %s", data, tt.wantData)
			}
			if encoding != tt.wantEncoding {
				t.Errorf("encoding = %q, want %q", encoding, tt.wantEncoding)
			}
		})
	}
}

func TestPageOptions(t *testing.T) {
	t.Parallel()

	opts := pageOptions(eventstore.Forward, 7, 50)
	if opts.AfterVersion != 7 || opts.Count != 51 || opts.Direction != eventstore.Forward {
		t.Errorf("forward opts = %+v, want AfterVersion=7 Count=51 Direction=Forward", opts)
	}

	opts = pageOptions(eventstore.Reverse, 0, 10)
	if opts.AfterVersion != 0 || opts.Count != 11 || opts.Direction != eventstore.Reverse {
		t.Errorf("reverse opts = %+v, want AfterVersion=0 Count=11 Direction=Reverse", opts)
	}
}

func TestPaginate(t *testing.T) {
	t.Parallel()

	versioned := func(versions ...int64) []eventJSON {
		events := make([]eventJSON, len(versions))
		for i, v := range versions {
			events[i] = eventJSON{Version: v}
		}
		return events
	}

	t.Run("forward with more pages", func(t *testing.T) {
		t.Parallel()
		// count=2, lookahead returned 3 events
		page, hasMore, nextAfter := paginate(versioned(1, 2, 3), 2, eventstore.Forward, streamCursor)
		if len(page) != 2 || page[1].Version != 2 {
			t.Errorf("page = %+v, want versions [1 2]", page)
		}
		if !hasMore {
			t.Error("hasMore = false, want true")
		}
		if nextAfter != 2 { // forward: next page reads events with version > 2
			t.Errorf("nextAfter = %d, want 2", nextAfter)
		}
	})

	t.Run("forward last page", func(t *testing.T) {
		t.Parallel()
		page, hasMore, nextAfter := paginate(versioned(3), 2, eventstore.Forward, streamCursor)
		if len(page) != 1 || hasMore {
			t.Errorf("page = %+v hasMore = %v, want 1 event and no more", page, hasMore)
		}
		if nextAfter != 3 {
			t.Errorf("nextAfter = %d, want 3", nextAfter)
		}
	})

	t.Run("reverse with more pages", func(t *testing.T) {
		t.Parallel()
		page, hasMore, nextAfter := paginate(versioned(5, 4, 3), 2, eventstore.Reverse, streamCursor)
		if len(page) != 2 || page[0].Version != 5 || page[1].Version != 4 {
			t.Errorf("page = %+v, want versions [5 4]", page)
		}
		if !hasMore {
			t.Error("hasMore = false, want true")
		}
		if nextAfter != 3 { // reverse: next page reads events with version <= 3
			t.Errorf("nextAfter = %d, want 3", nextAfter)
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		page, hasMore, nextAfter := paginate(nil, 2, eventstore.Forward, streamCursor)
		if len(page) != 0 || hasMore || nextAfter != 0 {
			t.Errorf("got page=%v hasMore=%v nextAfter=%d, want empty/false/0", page, hasMore, nextAfter)
		}
	})

	t.Run("global cursor", func(t *testing.T) {
		t.Parallel()
		pos := func(p int64) *int64 { return &p }
		events := []eventJSON{{GlobalPosition: pos(11)}, {GlobalPosition: pos(12)}, {GlobalPosition: pos(13)}}
		page, hasMore, nextAfter := paginate(events, 2, eventstore.Forward, globalCursor)
		if len(page) != 2 || !hasMore || nextAfter != 12 {
			t.Errorf("got %d events hasMore=%v nextAfter=%d, want 2/true/12", len(page), hasMore, nextAfter)
		}
	})
}

func TestRedactDSN(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"postgres://estoria:sekrit@localhost:5433/estoria?sslmode=disable", "postgres://estoria@localhost:5433/estoria?sslmode=disable"},
		{"postgres://estoria@localhost:5433/estoria", "postgres://estoria@localhost:5433/estoria"},
		{"../kanban/kanban.db", "../kanban/kanban.db"},
		{"host=localhost user=estoria password=sekrit dbname=estoria", "host=localhost user=estoria dbname=estoria"},
	}

	for _, tt := range tests {
		if got := redactDSN(tt.in); got != tt.want {
			t.Errorf("redactDSN(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ---- end-to-end over a real SQLite event store ----

// fixed UUIDs so stream ordering assertions are deterministic
var (
	widgetAID   = uuid.Must(uuid.FromString("11111111-1111-4000-8000-000000000001"))
	widgetBID   = uuid.Must(uuid.FromString("22222222-2222-4000-8000-000000000002"))
	gadgetID    = uuid.Must(uuid.FromString("33333333-3333-4000-8000-000000000003"))
	snapshotID  = uuid.Must(uuid.FromString("44444444-4444-4000-8000-000000000004"))
	unknownUUID = "99999999-9999-4000-8000-000000000009"
)

// seedTestStore creates a real SQLite event store in dir and appends a known
// set of streams to it, returning the database path. Append order determines
// global positions:
//
//	#1-#5: widget A v1-v5    #6-#7: gadget v1-v2 (v2 has a binary payload)
//	#8-#9: widget B v1-v2    #10-#11: foosnapshot v1-v2
func seedTestStore(t *testing.T, dir string) string {
	t.Helper()
	ctx := context.Background()

	path := filepath.Join(dir, "inspector_test.db")
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path))
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer db.Close()

	strat, err := sqlstrategy.NewDefaultStrategy()
	if err != nil {
		t.Fatalf("creating strategy: %v", err)
	}
	if _, err := db.ExecContext(ctx, strat.Schema()); err != nil {
		t.Fatalf("creating schema: %v", err)
	}

	store, err := sqlstore.New(db)
	if err != nil {
		t.Fatalf("creating event store: %v", err)
	}

	appendEvents := func(streamID typeid.ID, events ...*eventstore.WritableEvent) {
		t.Helper()
		if err := store.AppendStream(ctx, streamID, events, eventstore.AppendStreamOptions{}); err != nil {
			t.Fatalf("appending to %s: %v", streamID, err)
		}
	}

	jsonEvent := func(eventType string, n int) *eventstore.WritableEvent {
		return &eventstore.WritableEvent{Type: eventType, Data: fmt.Appendf(nil, `{"n":%d}`, n)}
	}

	appendEvents(typeid.New("widget", widgetAID),
		&eventstore.WritableEvent{Type: "widgetcreated", Data: []byte(`{"name":"first widget"}`), Metadata: map[string]string{"source": "test"}},
		jsonEvent("widgetrenamed", 2),
		jsonEvent("widgetrenamed", 3),
		jsonEvent("widgetrenamed", 4),
		jsonEvent("widgetrenamed", 5),
	)

	appendEvents(typeid.New("gadget", gadgetID),
		jsonEvent("gadgetcreated", 1),
		&eventstore.WritableEvent{Type: "gadgetblob", Data: []byte{0xff, 0xfe, 0x01}}, // deliberately not JSON
	)

	appendEvents(typeid.New("widget", widgetBID),
		jsonEvent("widgetcreated", 1),
		jsonEvent("widgetrenamed", 2),
	)

	appendEvents(typeid.New("foosnapshot", snapshotID),
		jsonEvent("foosnapshot", 1),
		jsonEvent("foosnapshot", 2),
	)

	return path
}

// getBody performs a GET and decodes the JSON response into a map.
func getBody(t *testing.T, ts *httptest.Server, path string, wantStatus int) map[string]any {
	t.Helper()
	res, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	if res.StatusCode != wantStatus {
		t.Fatalf("GET %s: status %d, want %d", path, res.StatusCode, wantStatus)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decoding %s response: %v", path, err)
	}
	return body
}

func eventVersions(t *testing.T, body map[string]any) []int64 {
	t.Helper()
	raw, ok := body["events"].([]any)
	if !ok {
		t.Fatalf("no events array in response: %v", body)
	}
	versions := make([]int64, len(raw))
	for i, e := range raw {
		versions[i] = int64(e.(map[string]any)["version"].(float64))
	}
	return versions
}

func TestInspectorEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	path := seedTestStore(t, t.TempDir())

	// connect through the production code path: the registry entry
	b, err := backends["sqlite"].connect(ctx, path)
	if err != nil {
		t.Fatalf("connecting sqlite backend: %v", err)
	}
	defer b.close()

	ts := httptest.NewServer((&server{backend: b}).routes())
	defer ts.Close()

	widgetA := "widget_" + widgetAID.String()

	t.Run("info", func(t *testing.T) {
		body := getBody(t, ts, "/api/info", http.StatusOK)
		if body["backend"] != "sqlite" || body["readOnly"] != true {
			t.Errorf("info = %v, want backend=sqlite readOnly=true", body)
		}
		caps := body["capabilities"].(map[string]any)
		if caps["listStreams"] != true || caps["readAll"] != true {
			t.Errorf("capabilities = %v, want both true", caps)
		}
	})

	t.Run("streams sorted by type then id", func(t *testing.T) {
		body := getBody(t, ts, "/api/streams", http.StatusOK)
		raw := body["streams"].([]any)
		if len(raw) != 4 {
			t.Fatalf("got %d streams, want 4", len(raw))
		}

		type row struct {
			id, typ string
			version int64
		}
		var rows []row
		for _, s := range raw {
			m := s.(map[string]any)
			rows = append(rows, row{m["id"].(string), m["type"].(string), int64(m["version"].(float64))})
		}

		want := []row{
			{"foosnapshot_" + snapshotID.String(), "foosnapshot", 2},
			{"gadget_" + gadgetID.String(), "gadget", 2},
			{"widget_" + widgetAID.String(), "widget", 5},
			{"widget_" + widgetBID.String(), "widget", 2},
		}
		for i, w := range want {
			if rows[i] != w {
				t.Errorf("stream[%d] = %+v, want %+v", i, rows[i], w)
			}
		}
	})

	t.Run("forward paging", func(t *testing.T) {
		body := getBody(t, ts, "/api/streams/"+widgetA+"/events?dir=forward&count=2", http.StatusOK)
		if got := eventVersions(t, body); len(got) != 2 || got[0] != 1 || got[1] != 2 {
			t.Errorf("page 1 versions = %v, want [1 2]", got)
		}
		if body["hasMore"] != true || body["nextAfter"] != float64(2) {
			t.Errorf("page 1 pager = hasMore:%v nextAfter:%v, want true/2", body["hasMore"], body["nextAfter"])
		}

		body = getBody(t, ts, "/api/streams/"+widgetA+"/events?dir=forward&count=2&after=2", http.StatusOK)
		if got := eventVersions(t, body); len(got) != 2 || got[0] != 3 || got[1] != 4 {
			t.Errorf("page 2 versions = %v, want [3 4]", got)
		}
		if body["hasMore"] != true || body["nextAfter"] != float64(4) {
			t.Errorf("page 2 pager = hasMore:%v nextAfter:%v, want true/4", body["hasMore"], body["nextAfter"])
		}

		body = getBody(t, ts, "/api/streams/"+widgetA+"/events?dir=forward&count=2&after=4", http.StatusOK)
		if got := eventVersions(t, body); len(got) != 1 || got[0] != 5 {
			t.Errorf("page 3 versions = %v, want [5]", got)
		}
		if body["hasMore"] != false {
			t.Errorf("page 3 hasMore = %v, want false", body["hasMore"])
		}
	})

	t.Run("reverse paging", func(t *testing.T) {
		// after=0 in reverse means "start from the latest event"
		body := getBody(t, ts, "/api/streams/"+widgetA+"/events?dir=reverse&count=2", http.StatusOK)
		if got := eventVersions(t, body); len(got) != 2 || got[0] != 5 || got[1] != 4 {
			t.Errorf("page 1 versions = %v, want [5 4]", got)
		}
		if body["hasMore"] != true || body["nextAfter"] != float64(3) {
			t.Errorf("page 1 pager = hasMore:%v nextAfter:%v, want true/3", body["hasMore"], body["nextAfter"])
		}

		body = getBody(t, ts, "/api/streams/"+widgetA+"/events?dir=reverse&count=2&after=3", http.StatusOK)
		if got := eventVersions(t, body); len(got) != 2 || got[0] != 3 || got[1] != 2 {
			t.Errorf("page 2 versions = %v, want [3 2]", got)
		}

		body = getBody(t, ts, "/api/streams/"+widgetA+"/events?dir=reverse&count=2&after=1", http.StatusOK)
		if got := eventVersions(t, body); len(got) != 1 || got[0] != 1 {
			t.Errorf("page 3 versions = %v, want [1]", got)
		}
		if body["hasMore"] != false {
			t.Errorf("page 3 hasMore = %v, want false", body["hasMore"])
		}
	})

	t.Run("payload and metadata shapes", func(t *testing.T) {
		body := getBody(t, ts, "/api/streams/"+widgetA+"/events?dir=forward&count=1", http.StatusOK)
		evt := body["events"].([]any)[0].(map[string]any)
		if evt["eventType"] != "widgetcreated" {
			t.Errorf("eventType = %v, want widgetcreated", evt["eventType"])
		}
		if evt["dataEncoding"] != "json" {
			t.Errorf("dataEncoding = %v, want json", evt["dataEncoding"])
		}
		if data := evt["data"].(map[string]any); data["name"] != "first widget" {
			t.Errorf("data = %v, want raw JSON with name field", data)
		}
		if meta := evt["metadata"].(map[string]any); meta["source"] != "test" {
			t.Errorf("metadata = %v, want source=test", meta)
		}
		if evt["globalPosition"] != float64(1) {
			t.Errorf("globalPosition = %v, want 1", evt["globalPosition"])
		}
	})

	t.Run("binary payload becomes base64", func(t *testing.T) {
		gadget := "gadget_" + gadgetID.String()
		body := getBody(t, ts, "/api/streams/"+gadget+"/events?dir=forward&count=10", http.StatusOK)
		evt := body["events"].([]any)[1].(map[string]any)
		if evt["dataEncoding"] != "base64" {
			t.Fatalf("dataEncoding = %v, want base64", evt["dataEncoding"])
		}
		decoded, err := base64.StdEncoding.DecodeString(evt["data"].(string))
		if err != nil || string(decoded) != string([]byte{0xff, 0xfe, 0x01}) {
			t.Errorf("data did not round-trip: %v (%v)", evt["data"], err)
		}
	})

	t.Run("unknown stream is 404", func(t *testing.T) {
		body := getBody(t, ts, "/api/streams/widget_"+unknownUUID+"/events", http.StatusNotFound)
		if body["error"] != "stream_not_found" {
			t.Errorf("error = %v, want stream_not_found", body["error"])
		}
	})

	t.Run("malformed stream ID is 400", func(t *testing.T) {
		getBody(t, ts, "/api/streams/nounderscore/events", http.StatusBadRequest)
		getBody(t, ts, "/api/streams/widget_notauuid/events", http.StatusBadRequest)
	})

	t.Run("global feed pages by global position", func(t *testing.T) {
		body := getBody(t, ts, "/api/all?count=4", http.StatusOK)
		raw := body["events"].([]any)
		if len(raw) != 4 {
			t.Fatalf("got %d events, want 4", len(raw))
		}
		for i, e := range raw {
			if pos := e.(map[string]any)["globalPosition"].(float64); pos != float64(i+1) {
				t.Errorf("event %d globalPosition = %v, want %d", i, pos, i+1)
			}
		}
		if body["hasMore"] != true || body["nextAfter"] != float64(4) {
			t.Errorf("pager = hasMore:%v nextAfter:%v, want true/4", body["hasMore"], body["nextAfter"])
		}

		// resume from the checkpoint: forward reads return positions > after
		body = getBody(t, ts, "/api/all?count=100&after=4", http.StatusOK)
		raw = body["events"].([]any)
		if len(raw) != 7 { // 11 total - 4 already seen
			t.Fatalf("got %d events after position 4, want 7", len(raw))
		}
		first := raw[0].(map[string]any)
		if first["globalPosition"] != float64(5) || first["streamId"] != "widget_"+widgetAID.String() {
			t.Errorf("first resumed event = %v, want widget A's v5 at position 5", first)
		}
		if body["hasMore"] != false || body["nextAfter"] != float64(11) {
			t.Errorf("pager = hasMore:%v nextAfter:%v, want false/11", body["hasMore"], body["nextAfter"])
		}

		// events from different streams interleave in append order
		gadgetFirst := raw[1].(map[string]any)
		if gadgetFirst["streamId"] != "gadget_"+gadgetID.String() || gadgetFirst["version"] != float64(1) {
			t.Errorf("event at position 6 = %v, want gadget v1", gadgetFirst)
		}
	})

	t.Run("global feed reads in reverse for tail bootstrap", func(t *testing.T) {
		body := getBody(t, ts, "/api/all?dir=reverse&count=3", http.StatusOK)
		raw := body["events"].([]any)
		positions := make([]float64, len(raw))
		for i, e := range raw {
			positions[i] = e.(map[string]any)["globalPosition"].(float64)
		}
		if len(positions) != 3 || positions[0] != 11 || positions[1] != 10 || positions[2] != 9 {
			t.Errorf("reverse positions = %v, want [11 10 9]", positions)
		}
		if body["hasMore"] != true || body["nextAfter"] != float64(8) {
			t.Errorf("pager = hasMore:%v nextAfter:%v, want true/8", body["hasMore"], body["nextAfter"])
		}
	})

	t.Run("tail polling past the end is empty, not an error", func(t *testing.T) {
		body := getBody(t, ts, "/api/all?after=999", http.StatusOK)
		if raw := body["events"].([]any); len(raw) != 0 {
			t.Errorf("got %d events past the end, want 0", len(raw))
		}
		if body["hasMore"] != false || body["nextAfter"] != float64(999) {
			t.Errorf("pager = hasMore:%v nextAfter:%v, want false/999", body["hasMore"], body["nextAfter"])
		}
	})

	t.Run("static UI", func(t *testing.T) {
		res, err := ts.Client().Get(ts.URL + "/")
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET / status = %d, want 200", res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET / content-type = %q, want text/html", ct)
		}
	})
}

// TestCapabilityDegradation exercises the inspector against a backend with NO
// optional capabilities: stream reads still work (they only need the core
// StreamReader), while stream listing and the global feed answer 501.
func TestCapabilityDegradation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	path := seedTestStore(t, t.TempDir())

	full, err := backends["sqlite"].connect(ctx, path)
	if err != nil {
		t.Fatalf("connecting sqlite backend: %v", err)
	}
	defer full.close()

	// same reader, but pretend the driver offered no extras
	degraded := &backend{
		name:   full.name,
		label:  full.label,
		dsn:    full.dsn,
		reader: full.reader,
		caps:   capabilities{}, // nil listStreams, nil readAll
		close:  func() {},
	}

	ts := httptest.NewServer((&server{backend: degraded}).routes())
	defer ts.Close()

	t.Run("info reports missing capabilities", func(t *testing.T) {
		body := getBody(t, ts, "/api/info", http.StatusOK)
		caps := body["capabilities"].(map[string]any)
		if caps["listStreams"] != false || caps["readAll"] != false {
			t.Errorf("capabilities = %v, want both false", caps)
		}
	})

	t.Run("streams answers 501", func(t *testing.T) {
		body := getBody(t, ts, "/api/streams", http.StatusNotImplemented)
		if body["error"] != "capability_unavailable" || body["capability"] != "listStreams" {
			t.Errorf("body = %v, want capability_unavailable/listStreams", body)
		}
	})

	t.Run("global feed answers 501", func(t *testing.T) {
		body := getBody(t, ts, "/api/all", http.StatusNotImplemented)
		if body["error"] != "capability_unavailable" || body["capability"] != "readAll" {
			t.Errorf("body = %v, want capability_unavailable/readAll", body)
		}
	})

	t.Run("core stream reads still work", func(t *testing.T) {
		widgetA := "widget_" + widgetAID.String()
		body := getBody(t, ts, "/api/streams/"+widgetA+"/events?dir=reverse&count=2", http.StatusOK)
		if got := eventVersions(t, body); len(got) != 2 || got[0] != 5 {
			t.Errorf("versions = %v, want [5 4]", got)
		}
	})
}

// TestSQLiteStartupValidation covers the read-only startup checks: the
// inspector never bootstraps a schema, so missing files and non-event-store
// databases must fail cleanly.
func TestSQLiteStartupValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		_, err := backends["sqlite"].connect(ctx, filepath.Join(t.TempDir(), "nope.db"))
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("err = %v, want does-not-exist error", err)
		}
	})

	t.Run("not an event store", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "other.db")
		db, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `CREATE TABLE unrelated (id INTEGER PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		db.Close()

		_, err = backends["sqlite"].connect(ctx, path)
		if err == nil || !strings.Contains(err.Error(), "missing table") {
			t.Errorf("err = %v, want missing-table error", err)
		}
	})
}
