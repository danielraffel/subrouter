package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// drainFirst shows, sets or clears the pool's drain-first list: Codex
// accounts every session uses before any other, to spend them down before
// their reset credits expire.
//
//	sr drain-first
//	sr drain-first ACCOUNT [ACCOUNT...] [--until 21:00|RFC3339]
//	sr drain-first --clear
func (r srRunner) drainFirst(args []string) error {
	serverURL, err := r.wakeServerURL()
	if err != nil {
		return err
	}
	url := strings.TrimRight(serverURL, "/") + "/_subrouter/drain-first"
	client := autoResumeHTTPClient()
	if len(args) == 0 {
		var drain proxy.DrainFirst
		if err := getAutoResumeJSON(serverURL, "/_subrouter/drain-first", &drain); err != nil {
			return err
		}
		if len(drain.Accounts) == 0 {
			fmt.Fprintln(r.out, "No drain-first list: Codex sessions are placed as usual.")
			return nil
		}
		until := "until cleared"
		if !drain.Until.IsZero() {
			until = "until " + drain.Until.Local().Format("Jan 2 15:04")
			if time.Now().After(drain.Until) {
				until = "expired at " + drain.Until.Local().Format("Jan 2 15:04") + " (no longer applied)"
			}
		}
		fmt.Fprintf(r.out, "Codex sessions on every machine use, in order, %s (%s).\nAn account with no quota left is skipped.\n", strings.Join(drain.Accounts, ", "), until)
		return nil
	}
	if args[0] == "--clear" {
		req, err := http.NewRequest(http.MethodDelete, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		fmt.Fprintln(r.out, "Drain-first cleared: Codex sessions are placed as usual.")
		return nil
	}
	var drain proxy.DrainFirst
	for i := 0; i < len(args); i++ {
		if args[i] == "--until" {
			if i+1 >= len(args) {
				return errors.New("--until needs a time, such as 21:00")
			}
			until, err := parseDrainUntil(args[i+1], time.Now())
			if err != nil {
				return err
			}
			drain.Until = until
			i++
			continue
		}
		drain.Accounts = append(drain.Accounts, args[i])
	}
	body, err := json.Marshal(drain)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("drain-first: %s", resp.Status)
	}
	fmt.Fprintf(r.out, "Codex sessions on every machine now use %s first, starting with their next request.\n", strings.Join(drain.Accounts, ", "))
	return nil
}

// parseDrainUntil accepts HH:MM (today, or tomorrow if already past) or RFC 3339.
func parseDrainUntil(value string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	clock, err := time.ParseInLocation("15:04", value, now.Location())
	if err != nil {
		return time.Time{}, fmt.Errorf("--until %q: use HH:MM or RFC 3339", value)
	}
	until := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, now.Location())
	if !until.After(now) {
		until = until.Add(24 * time.Hour)
	}
	return until, nil
}
