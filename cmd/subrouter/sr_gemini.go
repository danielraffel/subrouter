package main

import (
	"fmt"

	"github.com/manaflow-ai/subrouter/internal/agents/gemini"
)

const srGeminiHelp = `sr gemini - Manage Gemini profiles

Usage:
  sr gemini list      List stored Gemini profiles
  sr gemini help      Show this help

Gemini routing is namespaced separately in Subrouter. Login/import support will be added when the Gemini CLI credential format is wired in.
`

func (r srRunner) gemini(args []string) error {
	if len(args) == 0 {
		return r.geminiList()
	}
	switch args[0] {
	case "list", "ls", "status":
		return r.geminiList()
	case "help", "-h", "--help":
		fmt.Fprint(r.out, srGeminiHelp)
		return nil
	default:
		return fmt.Errorf("unknown command: sr gemini %s\n%s", args[0], srGeminiHelp)
	}
}

func (r srRunner) geminiList() error {
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
