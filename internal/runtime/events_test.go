package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"focus-test-runtime/internal/protocol"
)

func TestBoundedEventsDrainThousandsWithoutSkipping(t *testing.T) {
	j := &job{ID: "large", Result: protocol.ExecutionResult{Mode: "local-process", Status: "completed"}}
	for i := 1; i <= 5000; i++ {
		j.Events = append(j.Events, protocol.Event{JobID: j.ID, Seq: i, Kind: "case_failed", TestID: fmt.Sprint(i), Message: strings.Repeat("<故障>\n", 800)})
	}
	s := &Service{jobs: map[string]*job{j.ID: j}}
	cursor, pages := 0, 0
	for {
		page, err := s.ReadEvents(j.ID, protocol.EventQuery{AfterSeq: cursor, Limit: protocol.MaxEventLimit})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded)+1 > protocol.MaxEventPageBytes || len(page.Events) > protocol.MaxEventLimit {
			t.Fatal("unbounded wire page")
		}
		for _, event := range page.Events {
			if event.Seq != cursor+1 {
				t.Fatalf("cursor gap at %d: %+v", cursor, event)
			}
			cursor = event.Seq
		}
		if page.NextSeq != cursor {
			t.Fatal("next_seq skipped unseen events")
		}
		pages++
		if page.Terminal {
			if page.HasMore || cursor != 5000 {
				t.Fatal("premature terminal")
			}
			break
		}
		if !page.HasMore {
			t.Fatal("lost remaining events")
		}
	}
	if pages < 2 {
		t.Fatal("large stream was not paginated")
	}
	page, err := s.Events(j.ID, cursor)
	if err != nil || len(page.Events) != 0 || page.NextSeq != cursor || !page.Terminal {
		t.Fatalf("wrong drained cursor: %+v %v", page, err)
	}
	for _, q := range []protocol.EventQuery{{AfterSeq: -1, Limit: 1}, {AfterSeq: 5001, Limit: 1}, {Limit: 0}, {Limit: 257}} {
		if _, err := s.ReadEvents(j.ID, q); !errors.Is(err, ErrEventQuery) {
			t.Fatalf("accepted query %+v: %v", q, err)
		}
	}
}
