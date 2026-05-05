package gemini

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/manaflow-ai/subrouter/internal/storepath"
)

type Store struct {
	Dir string
}

type Profile struct {
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt"`
	LastUsed  string `json:"lastUsed,omitempty"`
	Dir       string `json:"dir,omitempty"`
}

type profilesFile struct {
	Active   string             `json:"active,omitempty"`
	Profiles map[string]Profile `json:"profiles"`
}

func DefaultStore() Store {
	return Store{Dir: storepath.CodexDir()}
}

func (s Store) ProfilesPath() string {
	return filepath.Join(s.Dir, "gemini.json")
}

func (s Store) ListProfiles() []Profile {
	data := s.readProfiles()
	out := make([]Profile, 0, len(data.Profiles))
	for _, profile := range data.Profiles {
		out = append(out, profile)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func (s Store) ActiveProfile() string {
	return s.readProfiles().Active
}

func (s Store) SetActiveProfile(name string) error {
	data := s.readProfiles()
	profile := data.Profiles[name]
	profile.LastUsed = time.Now().UTC().Format(time.RFC3339)
	data.Profiles[name] = profile
	data.Active = name
	return s.writeProfiles(data)
}

func (s Store) readProfiles() profilesFile {
	body, err := os.ReadFile(s.ProfilesPath())
	if err != nil {
		return profilesFile{Profiles: map[string]Profile{}}
	}
	var data profilesFile
	if err := json.Unmarshal(body, &data); err != nil {
		return profilesFile{Profiles: map[string]Profile{}}
	}
	if data.Profiles == nil {
		data.Profiles = map[string]Profile{}
	}
	return data
}

func (s Store) writeProfiles(data profilesFile) error {
	if data.Profiles == nil {
		data.Profiles = map[string]Profile{}
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(s.ProfilesPath(), body, 0o600)
}
