package main

import (
	"fmt"

	"github.com/manaflow-ai/subrouter/internal/agents/gemini"
)

const cxGeminiHelp = `cx gemini - Manage Gemini profiles

Usage:
  cx gemini list      List stored Gemini profiles
  cx gemini help      Show this help

Gemini routing is namespaced separately in Subrouter. Login/import support will be added when the Gemini CLI credential format is wired in.
`

func (r cxRunner) gemini(args []string) error {
	if len(args) == 0 {
		return r.geminiList()
	}
	switch args[0] {
	case "list", "ls", "status":
		return r.geminiList()
	case "help", "-h", "--help":
		fmt.Fprint(r.out, cxGeminiHelp)
		return nil
	default:
		return fmt.Errorf("unknown command: cx gemini %s\n%s", args[0], cxGeminiHelp)
	}
}

func (r cxRunner) geminiList() error {
	store := gemini.DefaultStore()
	profiles := store.ListProfiles()
	if len(profiles) == 0 {
		fmt.Fprintln(r.out, "No Gemini profiles configured yet.")
		return nil
	}
	active := store.ActiveProfile()
	fmt.Fprintln(r.out)
	for _, profile := range profiles {
		marker := ""
		if profile.Name == active {
			marker = " *"
		}
		fmt.Fprintf(r.out, "  %s%s (added %s)\n", profile.Name, marker, formatDate(profile.CreatedAt))
	}
	fmt.Fprintln(r.out)
	return nil
}
