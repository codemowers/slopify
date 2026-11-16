package main

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLocalSSEEvictsOldestOfThree(t *testing.T) {
	c := &controller{local: &localStatus{}, localSSEInterval: 10 * time.Second}
	server := httptest.NewServer(http.HandlerFunc(c.localEvents))
	defer server.Close()
	client := &http.Client{Timeout: 2 * time.Second}
	var oldest *bufio.Reader
	for i := 0; i < 4; i++ {
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("viewer %d: %d", i, response.StatusCode)
		}
		reader := bufio.NewReader(response.Body)
		line, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "data: ") {
			t.Fatalf("initial snapshot: %q %v", line, err)
		}
		reader.ReadString('\n')
		if i == 0 {
			oldest = reader
		}
	}
	line, err := oldest.ReadString('\n')
	if err != nil || line != "event: evicted\n" {
		t.Fatalf("oldest not evicted: %q %v", line, err)
	}
	c.local.eventsMu.Lock()
	count := len(c.local.viewers)
	c.local.eventsMu.Unlock()
	if count != 3 {
		t.Fatalf("active viewers: %d", count)
	}
}
